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
	// lockStale is how old a lock must be before another writer breaks it. A
	// writer holds one for milliseconds, so this is a crashed writer's.
	lockStale = 30 * time.Second

	// lockWait is how long a writer waits on one holder. A queue of writers
	// that keeps moving is waited out however long it is.
	lockWait = 10 * time.Second
)

// AppendKey adds a key to the account's enrolled file, creating it if need be.
// A key already in that file is not added twice, and added says so.
func (s *Store) AppendKey(account string, key ssh.PublicKey, comment string) (added bool, err error) {
	err = s.edit(account, func(name, path string) error {
		lines, err := readLines(path)
		if err != nil {
			return err
		}
		fp := ssh.FingerprintSHA256(key)
		for _, line := range lines {
			if k, _, isKey, _ := parseKeyLine([]byte(line)); isKey && ssh.FingerprintSHA256(k) == fp {
				return nil
			}
		}
		added = true
		return writeKeyFile(path, append(lines, keyLineFor(key, comment)))
	})
	return added, err
}

// CreateAccount enrols a key under a name nobody has: not in the uidmap and
// with no file in any directory. Of two concurrent creations of one name,
// exactly one succeeds and the other gets ErrAccountExists.
func (s *Store) CreateAccount(account string, key ssh.PublicKey, comment string) error {
	return s.edit(account, func(name, path string) error {
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
	for _, dir := range s.KeysDirs {
		path, ok, err := s.fileFor(dir, name)
		if err != nil {
			return false, err
		}
		if !ok {
			continue
		}
		lines, err := readLines(path)
		if err != nil {
			continue // unreadable is not a key to protect
		}
		for _, line := range lines {
			if k, _, isKey, _ := parseKeyLine([]byte(line)); isKey && ssh.FingerprintSHA256(k) == fingerprint {
				return false, &OperatorKeyError{What: "key " + fingerprint, File: path}
			}
		}
	}

	err = s.edit(name, func(_, path string) error {
		lines, err := readLines(path)
		if err != nil {
			return err
		}
		var kept []string
		for _, line := range lines {
			if k, _, isKey, _ := parseKeyLine([]byte(line)); isKey && ssh.FingerprintSHA256(k) == fingerprint {
				removed = true
				continue
			}
			kept = append(kept, line)
		}
		if !removed {
			return nil
		}
		return writeKeyFile(path, kept)
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
	for _, dir := range s.KeysDirs {
		path, ok, err := s.fileFor(dir, name)
		if err != nil {
			return err
		}
		if ok {
			return &OperatorKeyError{What: "a key file for " + name, File: path}
		}
	}
	return s.edit(name, func(_, path string) error {
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

// edit runs change on the account's enrolled file under its lock, then
// re-reads the accounts so the change is in force when edit returns. An
// account new to this process is in force once provisioned, which edit waits
// for up to ProvisionWait and then returns ErrProvisioning, the write made.
func (s *Store) edit(account string, change func(name, path string) error) error {
	if s.EnrolledDir == "" {
		return errNoEnrolledDir
	}
	name, err := workspace.AccountName(account)
	if err != nil {
		return err
	}
	path := filepath.Join(s.EnrolledDir, name+".pub")

	unlock, err := lockFile(filepath.Join(s.EnrolledDir, "."+name+".pub.lock"))
	if err != nil {
		return err
	}
	err = change(name, path)
	unlock()
	if err != nil {
		return err
	}
	if _, err := s.sync(); err != nil {
		return fmt.Errorf("%s was written, but the accounts were not re-read: %w", path, err)
	}
	return s.awaitProvisioning(name)
}

// lockFile takes a lock that holds across hosts: mkdir is atomic on NFS and
// CephFS as well as locally.
func lockFile(path string) (unlock func(), err error) {
	var holder, deadline time.Time // the lock's mtime names its holder
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
		if stale(path) {
			breakLock(path)
			continue
		}
		if info, err := os.Stat(path); err == nil && !info.ModTime().Equal(holder) {
			holder, deadline = info.ModTime(), time.Now().Add(lockWait)
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return nil, fmt.Errorf("%s is held by another writer", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// breakLock moves a crashed writer's lock aside.
//
// Under a second lock, and only once staleness is checked again under it: a
// writer that saw the lock stale may otherwise move it after another writer
// has broken it and taken it afresh, and both would then write.
func breakLock(path string) {
	breaker := path + ".break"
	if err := os.Mkdir(breaker, 0o700); err != nil {
		// A breaker that crashed in the microseconds it holds this.
		if os.IsExist(err) && stale(breaker) {
			_ = os.Remove(breaker)
		}
		time.Sleep(20 * time.Millisecond)
		return
	}
	defer func() { _ = os.Remove(breaker) }()

	if !stale(path) {
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

// keyLineFor is a key's line, its comment on one line whatever it was given:
// a newline in it would be a second line, and could be a second key.
func keyLineFor(key ssh.PublicKey, comment string) string {
	line := string(bytes.TrimSpace(ssh.MarshalAuthorizedKey(key)))
	if comment = strings.Join(strings.Fields(comment), " "); comment != "" {
		line += " " + comment
	}
	return line
}
