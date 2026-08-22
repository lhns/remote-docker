// Package accounts provisions one workspace account per enrolled public key.
//
// It replaces the key-watcher shell script. The model is unchanged, because it
// is a good one and deployments depend on it: a file named alice.pub becomes
// the unix account "alice", uids are allocated once and persisted so an
// account keeps the same uid, and therefore the same reverse-tunnel port and
// the same file ownership, across container recreations.
//
// What changes is that authentication happens in this process rather than
// through authorized_keys files, so port ownership can be enforced by a
// comparison rather than by a generated option string. See docs/adr/0010.
package accounts

import (
	"bufio"
	"bytes"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"

	"github.com/lhns/remote-docker/core/logx"
	"github.com/lhns/remote-docker/core/workspace"
)

// Account is one enrolled workspace user.
type Account struct {
	Name string
	UID  int
	GID  int
	Home string

	// Keys are the public keys enrolled for this account. A file may hold
	// several, one per line, and any of them authenticates. Empty means the
	// file was removed or emptied: access is revoked, but the account and its
	// home directory stay.
	Keys []ssh.PublicKey

	// Unix is the unix user behind this account, which is NOT its name:
	// `alice` is provisioned as `rd-alice` (ADR 0025). Anything asking the
	// operating system about the account -- its groups, what USER should say
	// in a shell -- has to ask about this one, and asking about Name instead
	// silently finds nothing rather than failing.
	Unix string
}

// Authorized reports whether a key may authenticate as this account.
func (a Account) Authorized(key ssh.PublicKey) bool {
	for _, k := range a.Keys {
		if ssh.FingerprintSHA256(k) == ssh.FingerprintSHA256(key) {
			return true
		}
	}
	return false
}

// Provisioner creates the unix account behind a workspace user.
//
// An interface because creating users needs root, which unit tests do not
// have. The shell suite could only run as root and therefore never ran in CI;
// this is what lets the interesting logic (naming, collisions, uid
// allocation, revocation) be tested anywhere.
type Provisioner interface {
	// Ensure creates the account if it does not exist.
	//
	// It returns the UNIX user it settled on as well as the home directory,
	// because that name is no longer derivable from the account name: `alice`
	// is provisioned as `rd-alice`, and an older workspace's `alice` is adopted
	// under the name it already has (ADR 0025).
	Ensure(name string, uid int, shell string) (unix, home string, err error)
}

// Store holds the accounts derived from a directory of public keys.
type Store struct {
	KeysDir  string
	StateDir string
	Shell    string
	Mapping  workspace.Mapping

	Provisioner Provisioner
	Log         *slog.Logger

	mu       sync.RWMutex
	accounts map[string]*Account

	// unusable is the accounts whose key file was present but held no usable
	// key on the previous sync. Revoking on one read cannot tell a deliberate
	// emptying from the middle of somebody's save, so it takes two.
	unusable map[string]bool
}

// New returns an empty store.
func New(keysDir, stateDir string, mapping workspace.Mapping, p Provisioner, log *slog.Logger) *Store {
	return &Store{
		KeysDir:     keysDir,
		StateDir:    stateDir,
		Shell:       "/bin/bash",
		Mapping:     mapping,
		Provisioner: p,
		Log:         log,
		accounts:    map[string]*Account{},
		unusable:    map[string]bool{},
	}
}

// Lookup returns an account by name.
func (s *Store) Lookup(name string) (*Account, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.accounts[name]
	return a, ok
}

// List returns every known account, ordered by name.
func (s *Store) List() []*Account {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]*Account, 0, len(s.accounts))
	for _, a := range s.accounts {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// uidmapPath is where allocated uids are persisted, in the same
// "name:uid" format the shell implementation used, so an existing deployment's
// uids must survive the change, because a uid determines both the account's
// reverse-tunnel port and the ownership of everything it has written.
func (s *Store) uidmapPath() string { return filepath.Join(s.StateDir, "uidmap") }

// Sync reads the keys directory and brings accounts into line with it.
func (s *Store) Sync() error {
	entries, err := os.ReadDir(s.KeysDir)
	if err != nil {
		return fmt.Errorf("accounts: reading %s: %w", s.KeysDir, err)
	}

	uids, err := s.loadUIDs()
	if err != nil {
		return err
	}

	// Sorted, so a collision is decided by name rather than by directory order,
	// which would make the winner depend on the filesystem.
	//
	// Files whose name is ALREADY the account name are considered first, so
	// alice.pub beats Alice.pub for "alice". Sorted order alone would hand it
	// to Alice.pub, because uppercase sorts first, which is deterministic but
	// arbitrary: the exact spelling is the one the enroller meant.
	var exact, folded []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".pub") {
			continue
		}
		base := strings.TrimSuffix(e.Name(), ".pub")
		if name, err := SanitizeName(base); err == nil && name == base {
			exact = append(exact, e.Name())
		} else {
			folded = append(folded, e.Name())
		}
	}
	sort.Strings(exact)
	sort.Strings(folded)
	names := append(exact, folded...)

	found := map[string]*Account{}
	claimed := map[string]string{} // account name -> file that claimed it
	unusable := map[string]bool{}  // account name -> its file is there and holds nothing

	for _, file := range names {
		base := strings.TrimSuffix(file, ".pub")
		name, err := SanitizeName(base)
		if err != nil {
			s.log().Warn("ignoring a key file", "file", file, "err", err)
			continue
		}

		// The shell version let a second file silently overwrite the first's
		// access: Alice.pub and alice.pub both yield "alice". Refusing is
		// the only safe answer: picking one would hand somebody an account
		// they did not ask for.
		if other, taken := claimed[name]; taken {
			s.log().Warn("ignoring a key file: its account name is already claimed",
				"file", file, "account", name, "claimedBy", other)
			continue
		}

		// Claimed before it is parsed. A file being written is empty for a
		// moment, and if the claim waited for a key then Alice.pub could take
		// "alice" during that moment, which is the takeover the rule above
		// exists to refuse.
		claimed[name] = file

		keys, skipped, err := parseKeys(filepath.Join(s.KeysDir, file))
		if err != nil {
			s.log().Warn("a key file holds no usable public key", "file", file, "err", err)
			unusable[name] = true
			continue
		}
		if skipped > 0 {
			s.log().Warn("a key file has lines that are not public keys; the rest are enrolled",
				"file", file, "keys", len(keys), "skipped", skipped)
		}

		found[name] = &Account{Name: name, Keys: keys}
	}

	return s.reconcile(found, unusable, uids)
}

// reconcile provisions new accounts and revokes ones whose key file no longer
// enrols them.
//
// unusable names the accounts whose file is still there but held no key this
// time round. See the revoke loop for why that is not the same thing as gone.
func (s *Store) reconcile(found map[string]*Account, unusable map[string]bool, uids map[string]int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	changed := false

	// Sorted, because this loop ASSIGNS uids to accounts that do not have one
	// yet, and ranging a map would assign them in Go's randomised order. Sync
	// goes to some trouble to order the key files deterministically; handing
	// the result over as a map threw that away, and the uid a new account got
	// (and therefore its reverse-tunnel port) depended on the run.
	for _, name := range slices.Sorted(maps.Keys(found)) {
		account := found[name]
		uid, ok := uids[name]
		if !ok {
			uid = nextUID(uids, s.Mapping.UIDBase)
			uids[name] = uid
			changed = true
		}
		account.UID = uid
		account.GID = uid

		unix, home, err := s.Provisioner.Ensure(name, uid, s.Shell)
		if err != nil {
			s.log().Error("could not provision an account", "account", name, "err", err)
			continue
		}
		account.Unix = unix
		account.Home = home

		if _, existed := s.accounts[name]; !existed {
			s.log().Info("account ready", "account", name, "uid", uid)
		}
		s.accounts[name] = account
	}

	// Revoke, do not delete. Removing the account and its home would be a
	// silent way to lose whatever the user left there, and a key file is
	// removed far more often than a person leaves for good.
	//
	// An account is enrolled exactly while its file holds a key, so emptying
	// the file revokes: that is the interface. But a file being saved is empty
	// for a moment, and one read cannot tell that moment from an emptying
	// meant on purpose. So a file that is THERE and holds nothing has to say so
	// twice, the second read being the next event or the next poll. A file that
	// is GONE revokes at once: there is no write window to be caught in.
	for name, account := range s.accounts {
		if _, still := found[name]; still {
			continue
		}
		if unusable[name] && !s.unusable[name] {
			continue
		}
		if len(account.Keys) > 0 {
			reason := "its key file is gone"
			if unusable[name] {
				reason = "its key file holds no usable public key"
			}
			s.log().Info("revoking an account: "+reason+". the account and its home are kept",
				"account", name)
		}
		account.Keys = nil
	}
	s.unusable = unusable

	if changed {
		if err := s.saveUIDs(uids); err != nil {
			return err
		}
	}
	return nil
}

// nextUID allocates the lowest free uid at or above the base.
func nextUID(uids map[string]int, base int) int {
	highest := base - 1
	for _, uid := range uids {
		if uid > highest {
			highest = uid
		}
	}
	return highest + 1
}

func (s *Store) loadUIDs() (map[string]int, error) {
	uids := map[string]int{}

	f, err := os.Open(s.uidmapPath())
	if err != nil {
		if os.IsNotExist(err) {
			return uids, nil
		}
		return nil, fmt.Errorf("accounts: reading uidmap: %w", err)
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		uid, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			continue
		}
		uids[strings.TrimSpace(name)] = uid
	}
	return uids, scanner.Err()
}

func (s *Store) saveUIDs(uids map[string]int) error {
	if err := os.MkdirAll(s.StateDir, 0o755); err != nil {
		return fmt.Errorf("accounts: creating state directory: %w", err)
	}

	names := make([]string, 0, len(uids))
	for name := range uids {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	for _, name := range names {
		fmt.Fprintf(&b, "%s:%d\n", name, uids[name])
	}

	// Written via a temporary file: a truncated uidmap would reallocate uids
	// on the next start, changing every account's port and orphaning the
	// ownership of everything already on disk.
	tmp := s.uidmapPath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("accounts: writing uidmap: %w", err)
	}
	if err := os.Rename(tmp, s.uidmapPath()); err != nil {
		return fmt.Errorf("accounts: replacing uidmap: %w", err)
	}
	return nil
}

// parseKeys reads every public key in a file, and counts the lines that were
// not one.
//
// Line by line, because a file holds several keys and one bad line should cost
// that line. Parsing the file as a stream stopped at the first thing it could
// not read, so a typo, a wrapped paste or a BOM on the top line took every key
// under it with it, and the account was revoked over a line nobody had touched.
func parseKeys(path string) (keys []ssh.PublicKey, skipped int, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}

	// A file saved by PowerShell's Out-File or by an older Notepad opens with a
	// byte order mark. UTF-8's is invisible in every editor and makes the first
	// line unreadable, so it is dropped; UTF-16 cannot be repaired here and is
	// named instead, because "no valid public key found" about a file that
	// plainly holds one sends the reader looking in the wrong place.
	if after, found := bytes.CutPrefix(data, []byte{0xef, 0xbb, 0xbf}); found {
		data = after
	} else if bytes.HasPrefix(data, []byte{0xff, 0xfe}) || bytes.HasPrefix(data, []byte{0xfe, 0xff}) {
		return nil, 0, fmt.Errorf("the file is UTF-16; save it as UTF-8")
	}

	scan := bufio.NewScanner(bytes.NewReader(data))
	for scan.Scan() {
		line := bytes.TrimSpace(scan.Bytes())
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		key, _, _, _, err := ssh.ParseAuthorizedKey(line)
		if err != nil {
			skipped++
			continue
		}
		keys = append(keys, key)
	}
	if err := scan.Err(); err != nil {
		return nil, 0, err
	}

	if len(keys) == 0 {
		if skipped == 0 {
			return nil, 0, fmt.Errorf("the file is empty")
		}
		return nil, skipped, fmt.Errorf("none of its %d lines is a public key", skipped)
	}
	return keys, skipped, nil
}

// log is the store's logger, or silence. See logx.Or.
func (s *Store) log() *slog.Logger {
	return logx.Or(s.Log)
}
