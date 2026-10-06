package accounts

// The writer of the enrolled keys directory, the one directory the agent
// writes (ADR 0052). Every change takes a per-file lock, rewrites the file
// whole through a temporary and a rename, and re-reads the accounts before it
// returns. A hand edit in that directory bypasses the lock; hand edits belong
// in an operator directory.

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/lhns/remote-docker/core/workspace"
)

var (
	errNoEnrolledDir = errors.New("this workspace has no enrolled keys directory")

	// ErrAccountExists refuses CreateAccount a name already taken.
	ErrAccountExists = errors.New("the account already exists")

	// ErrProvisioning is a write that succeeded for an account the workspace
	// is still creating: the key authenticates once that finishes.
	ErrProvisioning = errors.New("the account is still being created on the workspace")

	// ErrNotProvisioned is a key that cannot authenticate because the
	// workspace could not create its account; one the call added is taken out
	// again. The agent's log has the reason.
	ErrNotProvisioned = errors.New("the workspace could not create the account")
)

// OperatorKeyError refuses a change only the operator's directory could make.
type OperatorKeyError struct {
	What string // "key SHA256:...", or "a key file for alice"
	File string
}

func (e *OperatorKeyError) Error() string {
	return fmt.Sprintf("%s is in %s, which this workspace does not write", e.What, e.File)
}

// Lock timing. Variables so a test can shorten them.
var (
	// lockWait is how long a writer waits on one holder before it gives up, or
	// breaks the lock if it is stale. A queue of writers that keeps moving is
	// waited out however long it is.
	lockWait = 10 * time.Second

	// lockStale is how old a lock's mtime must be for it to be a crashed
	// writer's: a live writer holds one for milliseconds.
	lockStale = 30 * time.Second
)

// AppendKey adds a key to the account's enrolled file, creating it if need be.
// A key already in that file is not added twice, and added says so.
func (s *Store) AppendKey(account string, key ssh.PublicKey, comment string) (added bool, err error) {
	name, err := workspace.AccountName(account)
	if err != nil {
		return false, err
	}
	fp := ssh.FingerprintSHA256(key)
	err = s.edit(name, func(path string) error {
		lines, err := readLines(path)
		if err != nil {
			return err
		}
		if _, found := withoutKey(lines, fp); found {
			return nil
		}
		added = true
		return writeKeyFile(path, append(lines, keyLineFor(key, comment)))
	})
	if err != nil {
		return false, err
	}
	if !added {
		return false, s.awaitProvisioning(name)
	}
	return true, s.awaitOrWithdraw(name, fp)
}

// CreateAccount enrols a key under a name nobody has: not in the uidmap and
// with no file in any directory. Of two concurrent creations of one name,
// exactly one succeeds and the other gets ErrAccountExists.
func (s *Store) CreateAccount(account string, key ssh.PublicKey, comment string) error {
	name, err := workspace.AccountName(account)
	if err != nil {
		return err
	}
	err = s.edit(name, func(path string) error {
		known, err := s.Known(name)
		if err != nil {
			return err
		}
		if known {
			return ErrAccountExists
		}

		tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
		if err != nil {
			return err
		}
		defer func() { _ = os.Remove(tmp.Name()) }()
		if _, err := tmp.WriteString(keyLineFor(key, comment) + "\n"); err != nil {
			_ = tmp.Close()
			return err
		}
		if err := tmp.Sync(); err != nil {
			_ = tmp.Close()
			return err
		}
		if err := tmp.Close(); err != nil {
			return err
		}
		if err := os.Chmod(tmp.Name(), 0o644); err != nil {
			return err
		}
		// A link, not a rename: it refuses a file that is already there, even
		// one written by hand without the lock.
		if err := os.Link(tmp.Name(), path); err != nil {
			if os.IsExist(err) {
				return ErrAccountExists
			}
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	return s.awaitOrWithdraw(name, ssh.FingerprintSHA256(key))
}

// RemoveKey takes a key, by fingerprint, out of the account's enrolled file,
// and deletes the file if no key is left in it. A key that is also in an
// operator directory is refused, since removing only this copy would leave it
// working.
func (s *Store) RemoveKey(account, fingerprint string) (removed bool, err error) {
	name, err := workspace.AccountName(account)
	if err != nil {
		return false, err
	}
	files, err := s.operatorFiles(name)
	if err != nil {
		return false, err
	}
	for _, path := range files {
		lines, err := readLines(path)
		if err != nil {
			continue // unreadable is not a key to protect
		}
		if _, found := withoutKey(lines, fingerprint); found {
			return false, &OperatorKeyError{What: "key " + fingerprint, File: path}
		}
	}

	err = s.edit(name, func(path string) error {
		removed, err = removeKeyLine(path, fingerprint)
		return err
	})
	return removed, err
}

// RemoveAccountFile deletes the account's enrolled file, which revokes the
// account at once unless an operator directory enrols it too, and that is
// refused instead.
func (s *Store) RemoveAccountFile(account string) error {
	name, err := workspace.AccountName(account)
	if err != nil {
		return err
	}
	files, err := s.operatorFiles(name)
	if err != nil {
		return err
	}
	if len(files) > 0 {
		return &OperatorKeyError{What: "a key file for " + name, File: files[0]}
	}
	return s.edit(name, func(path string) error {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	})
}

// Known reports whether a name is taken: the uidmap has it, which is forever,
// or some directory has a file for it.
func (s *Store) Known(account string) (bool, error) {
	name, err := workspace.AccountName(account)
	if err != nil {
		return false, err
	}
	s.syncMu.Lock() // a sync may be replacing the uidmap; never held across a useradd
	uids, err := s.loadUIDs()
	s.syncMu.Unlock()
	if err != nil {
		return false, err
	}
	if _, ok := uids[name]; ok {
		return true, nil
	}
	for _, dir := range s.dirs() {
		_, ok, err := s.fileFor(dir, name)
		if err != nil || ok {
			return ok, err
		}
	}
	return false, nil
}

// fileFor is the file in dir that enrols the account, if any.
func (s *Store) fileFor(dir, name string) (string, bool, error) {
	files, err := s.keyFiles(dir)
	if err != nil {
		return "", false, err
	}
	for _, f := range files {
		if f.account == name {
			return filepath.Join(dir, f.file), true, nil
		}
	}
	return "", false, nil
}

// operatorFiles is every operator directory's file for the account.
func (s *Store) operatorFiles(name string) ([]string, error) {
	var out []string
	for _, dir := range s.KeysDirs {
		path, ok, err := s.fileFor(dir, name)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, path)
		}
	}
	return out, nil
}

// edit runs change on the enrolled file of the account name, which is already
// folded, under its lock, then re-reads the accounts so the change is in force
// when edit returns, except for an account still to be provisioned: see
// awaitOrWithdraw.
func (s *Store) edit(name string, change func(path string) error) error {
	if s.EnrolledDir == "" {
		return errNoEnrolledDir
	}
	path := filepath.Join(s.EnrolledDir, name+".pub")

	unlock, err := lockFile(filepath.Join(s.EnrolledDir, "."+name+".pub.lock"))
	if err != nil {
		return err
	}
	err = change(path)
	unlock()
	if err != nil {
		return err
	}
	if _, err := s.sync(); err != nil {
		return fmt.Errorf("%s was written, but the accounts were not re-read: %w", path, err)
	}
	return nil
}

// awaitOrWithdraw waits for the account a key was just added to. Still being
// created after ProvisionWait is ErrProvisioning, the key kept; never created
// is ErrNotProvisioned, the key taken out again, since the caller is about to
// say the key did not enrol.
func (s *Store) awaitOrWithdraw(name, fingerprint string) error {
	err := s.awaitProvisioning(name)
	if !errors.Is(err, ErrNotProvisioned) {
		return err
	}
	undo := s.edit(name, func(path string) error {
		_, err := removeKeyLine(path, fingerprint)
		return err
	})
	if undo != nil {
		return fmt.Errorf("%w, and its key could not be taken out again: %w", err, undo)
	}
	return err
}

// lockFile takes a lock that holds across hosts: mkdir is atomic on NFS and
// CephFS as well as locally.
//
// A lock is broken only once this writer has watched the same holder for
// lockWait AND its mtime is older than lockStale. The mtime is the
// filesystem's clock and time.Since is this host's, so on shared storage a
// clock running ahead makes every live lock look old; the watch is what still
// keeps two writers out.
func lockFile(path string) (unlock func(), err error) {
	var holder, since time.Time // the lock's mtime names its holder
	broke := false
	for {
		err := os.Mkdir(path, 0o700)
		if err == nil {
			return func() { _ = os.Remove(path) }, nil
		}
		// Windows answers a mkdir racing another writer's unlock with access
		// denied; only the unit tests run there.
		if !os.IsExist(err) && (runtime.GOOS != "windows" || !os.IsPermission(err)) {
			return nil, err
		}
		// A failed stat is a lock released in between. Every retry sleeps: on
		// Windows a stat holds the directory open, and a removed directory
		// someone holds open stays in the way of the next mkdir.
		if info, err := os.Stat(path); err == nil {
			if !info.ModTime().Equal(holder) {
				holder, since, broke = info.ModTime(), time.Now(), false
			}
			if time.Since(since) > lockWait {
				if broke || time.Since(holder) <= lockStale {
					return nil, fmt.Errorf("%s is held by another writer", path)
				}
				breakLock(path, holder)
				broke, since = true, time.Now()
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// breakLock moves a crashed writer's lock aside, if it is still the holder the
// caller watched.
//
// Under a second lock, and only once the holder is checked again under it:
// two writers may both have watched it, and the second must not move the lock
// the first broke and somebody has taken afresh.
func breakLock(path string, holder time.Time) {
	breaker := path + ".break"
	if err := os.Mkdir(breaker, 0o700); err != nil {
		// A breaker that crashed in the microseconds it holds this. One that
		// only looks old is still kept to the holder by the check below.
		if os.IsExist(err) && stale(breaker) {
			_ = os.Remove(breaker)
		}
		time.Sleep(20 * time.Millisecond)
		return
	}
	defer func() { _ = os.Remove(breaker) }()

	if info, err := os.Stat(path); err != nil || !info.ModTime().Equal(holder) {
		return
	}
	aside := fmt.Sprintf("%s.stale-%d-%d", path, os.Getpid(), time.Now().UnixNano())
	if os.Rename(path, aside) == nil {
		_ = os.RemoveAll(aside)
	}
}

// stale reports whether a lock is older than any live writer holds one.
func stale(path string) bool {
	info, err := os.Stat(path)
	return err == nil && time.Since(info.ModTime()) > lockStale
}

// readLines is a key file's lines as they are, or none for a missing file.
func readLines(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if data, err = stripBOM(data); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(data) == 0 {
		return nil, nil
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n"), nil
}

// writeKeyFile replaces a key file, or deletes it when no key is left: a
// missing file revokes at once, where an empty one takes two reads.
func writeKeyFile(path string, lines []string) error {
	for _, line := range lines {
		if _, _, isKey, _ := parseKeyLine([]byte(line)); isKey {
			return WriteRecord(path, lines, 0o644)
		}
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// withoutKey is lines less every line holding the key, and whether there was one.
func withoutKey(lines []string, fingerprint string) (kept []string, found bool) {
	for _, line := range lines {
		if k, _, isKey, _ := parseKeyLine([]byte(line)); isKey && ssh.FingerprintSHA256(k) == fingerprint {
			found = true
			continue
		}
		kept = append(kept, line)
	}
	return kept, found
}

// removeKeyLine takes a key out of a key file.
func removeKeyLine(path, fingerprint string) (bool, error) {
	lines, err := readLines(path)
	if err != nil {
		return false, err
	}
	kept, found := withoutKey(lines, fingerprint)
	if !found {
		return false, nil
	}
	return true, writeKeyFile(path, kept)
}

// keyLineFor is a key's line, its comment on one line whatever it was given:
// a newline in it would be a second line, and could be a second key.
func keyLineFor(key ssh.PublicKey, comment string) string {
	line := string(bytes.TrimSpace(ssh.MarshalAuthorizedKey(key)))
	if comment = strings.Join(strings.Fields(comment), " "); comment != "" {
		line += " " + comment
	}
	return line
}
