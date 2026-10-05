// Package accounts provisions one workspace account per enrolled public key.
//
// A file named alice.pub, in any of the keys directories (ADR 0052), becomes
// the account "alice"; uids are allocated once
// and persisted, so an account keeps the same uid, and therefore the same
// reverse-tunnel port and the same file ownership, across container
// recreations. Authentication happens in this process, so port ownership is a
// comparison rather than a generated authorized_keys option (ADR 0010).
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

	// Keys are the public keys enrolled for this account: the union of every
	// directory's file for it, one key per fingerprint. A file may hold
	// several, one per line, and any of them authenticates. Empty means no
	// directory enrols it any more: access is revoked, but the account and its
	// home directory stay.
	Keys []ssh.PublicKey

	// Sources are what each directory contributed to Keys, in directory order.
	Sources []KeySource

	// Unix is the unix user behind this account, which is NOT its name:
	// `alice` is provisioned as `rd-alice` (ADR 0025). Anything asking the
	// operating system about the account -- its groups, what USER should say
	// in a shell -- has to ask about this one, and asking about Name instead
	// silently finds nothing rather than failing.
	Unix string
}

// KeySource is one directory's file for an account.
type KeySource struct {
	Dir      string
	Keys     []ssh.PublicKey
	Comments []string // parallel to Keys
}

// source returns what dir contributed to the account, if anything.
func (a *Account) source(dir string) KeySource {
	if a != nil {
		for _, src := range a.Sources {
			if src.Dir == dir {
				return src
			}
		}
	}
	return KeySource{}
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
// have, and naming, collisions, uid allocation and revocation all have to be
// testable on a machine that is not a workspace.
type Provisioner interface {
	// Ensure creates the account if it does not exist.
	//
	// It returns the UNIX user it settled on as well as the home directory,
	// because that name is no longer derivable from the account name: `alice`
	// is provisioned as `rd-alice`, and an older workspace's `alice` is adopted
	// under the name it already has (ADR 0025).
	Ensure(name string, uid int, shell string) (unix, home string, err error)
}

// Store holds the accounts derived from the keys directories.
type Store struct {
	// KeysDirs are the operator's directories, which the agent only reads.
	KeysDirs []string

	// EnrolledDir is the one directory the agent writes, through
	// keyfile_write.go. Empty means there is none and every write is refused.
	EnrolledDir string

	StateDir string
	Shell    string
	Mapping  workspace.Mapping

	Provisioner Provisioner
	Log         *slog.Logger

	// syncMu serialises Sync end to end. mu is not held across the uid
	// allocation, so without this two syncs would each load their own uid map,
	// each call nextUID, and hand one uid to two accounts.
	syncMu sync.Mutex

	mu sync.RWMutex

	// An *Account in here is IMMUTABLE once published: Lookup hands the
	// pointer to the SSH authenticator, which ranges Keys with no
	// synchronisation (agent/internal/sshd/server.go). A change, revocation
	// included, means a new *Account in a new map.
	accounts map[string]*Account

	// unusable is the key files that were present but held no usable key on
	// the previous sync. Revoking on one read cannot tell a deliberate emptying
	// from the middle of somebody's save, so it takes two, per directory.
	unusable map[keyFile]bool
}

// keyFile is one account's file in one directory.
type keyFile struct{ dir, account string }

// New returns an empty store.
func New(keysDirs []string, enrolledDir, stateDir string, mapping workspace.Mapping, p Provisioner, log *slog.Logger) *Store {
	return &Store{
		KeysDirs:    keysDirs,
		EnrolledDir: enrolledDir,
		StateDir:    stateDir,
		Shell:       "/bin/bash",
		Mapping:     mapping,
		Provisioner: p,
		Log:         log,
		accounts:    map[string]*Account{},
		unusable:    map[keyFile]bool{},
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

// dirs is every keys directory: the operator's first, the enrolled one last.
func (s *Store) dirs() []string {
	if s.EnrolledDir == "" {
		return s.KeysDirs
	}
	return append(slices.Clone(s.KeysDirs), s.EnrolledDir)
}

// Sync reads every keys directory and brings accounts into line with them.
//
// Each directory is read by the same rules and the results are merged by
// account, Keys being the union deduplicated by fingerprint (ADR 0052).
func (s *Store) Sync() error {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()

	uids, err := s.loadUIDs()
	if err != nil {
		return err
	}

	// Read without mu: only a sync writes these, and syncMu is held.
	prev, wasUnusable := s.accounts, s.unusable

	found := map[string]*Account{}
	unusable := map[keyFile]bool{}

	for _, dir := range s.dirs() {
		files, err := s.keyFiles(dir)
		if err != nil {
			return err
		}
		for _, f := range files {
			keys, comments, skipped, err := parseKeys(filepath.Join(dir, f.file))
			src := KeySource{Dir: dir, Keys: keys, Comments: comments}
			if err != nil {
				s.log().Warn("a key file holds no usable public key", "dir", dir, "file", f.file, "err", err)
				id := keyFile{dir, f.account}
				unusable[id] = true
				if wasUnusable[id] {
					continue // the second read: it contributes nothing
				}
				// The first read may be the middle of a save, so the file
				// contributes what it did last time.
				if src = prev[f.account].source(dir); len(src.Keys) == 0 {
					continue
				}
			} else if skipped > 0 {
				s.log().Warn("a key file has lines that are not public keys; the rest are enrolled",
					"dir", dir, "file", f.file, "keys", len(keys), "skipped", skipped)
			}

			a, ok := found[f.account]
			if !ok {
				a = &Account{Name: f.account}
				found[f.account] = a
			}
			a.Sources = append(a.Sources, src)
		}
	}

	for _, a := range found {
		seen := map[string]bool{}
		for _, src := range a.Sources {
			for _, k := range src.Keys {
				if fp := ssh.FingerprintSHA256(k); !seen[fp] {
					seen[fp] = true
					a.Keys = append(a.Keys, k)
				}
			}
		}
	}

	return s.reconcile(found, unusable, uids)
}

// namedFile is a key file and the account it enrols.
type namedFile struct{ file, account string }

// keyFiles lists one directory's key files and the account each enrols.
//
// A missing enrolled directory is empty, because the agent serves without one;
// a missing operator directory is an error, as it always was (ADR 0052).
func (s *Store) keyFiles(dir string) ([]namedFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) && dir == s.EnrolledDir {
			return nil, nil
		}
		return nil, fmt.Errorf("accounts: reading %s: %w", dir, err)
	}

	// Sorted, so a collision is decided by name rather than by directory order,
	// which would make the winner depend on the filesystem. Files whose name is
	// ALREADY the account name go first, so alice.pub beats Alice.pub for
	// "alice": sorted order alone would hand it to Alice.pub, uppercase sorting
	// first, which is deterministic but arbitrary.
	//
	// Dotfiles are skipped: the writer's locks and temporary files are dotfiles.
	var exact, folded []string
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") || !strings.HasSuffix(e.Name(), ".pub") {
			continue
		}
		base := strings.TrimSuffix(e.Name(), ".pub")
		if name, err := workspace.AccountName(base); err == nil && name == base {
			exact = append(exact, e.Name())
		} else {
			folded = append(folded, e.Name())
		}
	}
	sort.Strings(exact)
	sort.Strings(folded)

	var out []namedFile
	claimed := map[string]string{} // account name -> file that claimed it
	for _, file := range append(exact, folded...) {
		name, err := workspace.AccountName(strings.TrimSuffix(file, ".pub"))
		if err != nil {
			s.log().Warn("ignoring a key file", "dir", dir, "file", file, "err", err)
			continue
		}

		// Alice.pub and alice.pub both yield "alice", and picking one would
		// hand somebody an account they did not ask for.
		//
		// Claimed before it is parsed. A file being written is empty for a
		// moment, and if the claim waited for a key then Alice.pub could take
		// "alice" during that moment, which is the takeover this refuses.
		if other, taken := claimed[name]; taken {
			s.log().Warn("ignoring a key file: its account name is already claimed",
				"dir", dir, "file", file, "account", name, "claimedBy", other)
			continue
		}
		claimed[name] = file
		out = append(out, namedFile{file, name})
	}
	return out, nil
}

// reconcile provisions new accounts and revokes ones no directory enrols any
// more.
//
// Sync has already applied the two-read rule; unusable is only recorded for
// the next sync and used to name the reason for a revocation.
//
// Four phases, and only the last holds s.mu, because provisioning shells out
// to useradd per account and Lookup reads through that lock. See the syncMu
// and accounts fields.
func (s *Store) reconcile(found map[string]*Account, unusable map[keyFile]bool, uids map[string]int) error {
	// 1. Decide the uids. No lock and no exec.
	//
	// Sorted, because this loop ASSIGNS uids to accounts that do not have one
	// yet, and ranging a map would assign them in Go's randomised order: Sync
	// orders the key files deterministically and handing the result over as a
	// map threw that away, so the uid a new account got, and therefore its
	// reverse-tunnel port, depended on the run.
	names := slices.Sorted(maps.Keys(found))
	changed := false
	for _, name := range names {
		account := found[name]
		uid, ok := uids[name]
		if !ok {
			uid = nextUID(uids, s.Mapping.UIDBase)
			uids[name] = uid
			changed = true
		}
		account.UID = uid
		account.GID = uid
	}

	// Persisted before anything is provisioned. Crashing in between leaves a
	// uid allocated to an account that does not exist yet, which costs
	// nothing: nextUID is highest+1 and never reuses one.
	if changed {
		if err := s.saveUIDs(uids); err != nil {
			return err
		}
	}

	// 2. Provision, still with no lock held and in the same order.
	failed := map[string]bool{}
	for _, name := range names {
		account := found[name]
		unix, home, err := s.Provisioner.Ensure(name, account.UID, s.Shell)
		if err != nil {
			s.log().Error("could not provision an account", "account", name, "err", err)
			failed[name] = true
			continue
		}
		account.Unix = unix
		account.Home = home
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// 3. Build the next map, seeded from the current one so an account whose
	// Ensure failed this round is carried forward and keeps authenticating: a
	// transient useradd failure must not present as a key that stopped working.
	// With the files' keys, though, or a key replaced in one would keep working.
	next := make(map[string]*Account, len(s.accounts)+len(found))
	maps.Copy(next, s.accounts)
	for _, name := range names {
		if failed[name] {
			if known, ok := s.accounts[name]; ok {
				carried := *known
				carried.Keys = found[name].Keys
				carried.Sources = found[name].Sources
				next[name] = &carried
			}
			continue
		}
		if _, existed := s.accounts[name]; !existed {
			s.log().Info("account ready", "account", name, "uid", found[name].UID)
		}
		next[name] = found[name]
	}

	// Revoke, do not delete. Removing the account and its home would silently
	// lose whatever the user left there, and a key file is removed far more
	// often than a person leaves for good.
	//
	// An account is enrolled exactly while some file holds a key for it, so
	// emptying the files revokes: that is the interface. But a file being
	// saved is empty for a moment, and one read cannot tell that moment from an
	// emptying meant on purpose. So a file that is THERE and holds nothing has
	// to say so twice, which Sync decides per directory. A file that is GONE
	// revokes at once: there is no write window to be caught in.
	//
	// A COPY with no keys, never Keys=nil on an account already published: see
	// the accounts field.
	for name, account := range next {
		if _, still := found[name]; still {
			continue
		}
		if len(account.Keys) == 0 {
			continue
		}
		reason := "its key file is gone"
		for id := range unusable {
			if id.account == name {
				reason = "its key file holds no usable public key"
			}
		}
		s.log().Info("revoking an account: "+reason+". the account and its home are kept",
			"account", name)

		revoked := *account
		revoked.Keys = nil
		revoked.Sources = nil
		next[name] = &revoked
	}

	// 4. Swap. s.unusable is what Sync's two-read rule reads next time, so it
	// changes with the map it was computed against.
	s.accounts = next
	s.unusable = unusable
	return nil
}

// nextUID allocates one above the highest uid in the record, and at least the
// base. Never the lowest free one, so no uid is handed out twice.
func nextUID(uids map[string]int, base int) int {
	highest := base - 1
	for _, uid := range uids {
		highest = max(highest, uid)
	}
	return highest + 1
}

func (s *Store) loadUIDs() (map[string]int, error) {
	uids := map[string]int{}
	err := ReadRecord(s.uidmapPath(), func(line string) {
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return
		}
		if uid, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
			uids[strings.TrimSpace(name)] = uid
		}
	})
	if err != nil {
		return nil, fmt.Errorf("accounts: reading uidmap: %w", err)
	}
	return uids, nil
}

func (s *Store) saveUIDs(uids map[string]int) error {
	if err := os.MkdirAll(s.StateDir, 0o755); err != nil {
		return fmt.Errorf("accounts: creating state directory: %w", err)
	}

	lines := make([]string, 0, len(uids))
	for _, name := range slices.Sorted(maps.Keys(uids)) {
		lines = append(lines, fmt.Sprintf("%s:%d", name, uids[name]))
	}
	if err := WriteRecord(s.uidmapPath(), lines, 0o644); err != nil {
		return fmt.Errorf("accounts: writing uidmap: %w", err)
	}
	return nil
}

// parseKeys reads every public key in a file, and counts the lines that were
// not one.
//
// Line by line, because a file holds several keys and one bad line should cost
// that line. Read as one stream it stopped at the first thing it could not
// parse, so a typo, a wrapped paste or a BOM on the top line took every key
// under it, and the account was revoked over a line nobody had touched.
func parseKeys(path string) (keys []ssh.PublicKey, comments []string, skipped int, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, 0, err
	}

	data, err = stripBOM(data)
	if err != nil {
		return nil, nil, 0, err
	}

	scan := bufio.NewScanner(bytes.NewReader(data))
	for scan.Scan() {
		key, comment, isKey, blank := parseKeyLine(scan.Bytes())
		switch {
		case blank:
		case !isKey:
			skipped++
		default:
			keys = append(keys, key)
			comments = append(comments, comment)
		}
	}
	if err := scan.Err(); err != nil {
		return nil, nil, 0, err
	}

	if len(keys) == 0 {
		if skipped == 0 {
			return nil, nil, 0, fmt.Errorf("the file is empty")
		}
		return nil, nil, skipped, fmt.Errorf("none of its %d lines is a public key", skipped)
	}
	return keys, comments, skipped, nil
}

// stripBOM drops a UTF-8 byte order mark and refuses UTF-16.
//
// A file saved by PowerShell's Out-File or by an older Notepad opens with a
// byte order mark. UTF-8's is invisible in every editor and makes the first
// line unreadable, so it is dropped; UTF-16 cannot be repaired here and is
// named instead, because "no valid public key found" about a file that
// plainly holds one sends the reader looking in the wrong place.
func stripBOM(data []byte) ([]byte, error) {
	if after, found := bytes.CutPrefix(data, []byte{0xef, 0xbb, 0xbf}); found {
		return after, nil
	}
	if bytes.HasPrefix(data, []byte{0xff, 0xfe}) || bytes.HasPrefix(data, []byte{0xfe, 0xff}) {
		return nil, fmt.Errorf("the file is UTF-16; save it as UTF-8")
	}
	return data, nil
}

// parseKeyLine reads one line of a key file. blank is a line that is empty or a
// comment; otherwise isKey says whether it held a public key.
func parseKeyLine(raw []byte) (key ssh.PublicKey, comment string, isKey, blank bool) {
	line := bytes.TrimSpace(raw)
	if len(line) == 0 || line[0] == '#' {
		return nil, "", false, true
	}
	key, comment, _, _, err := ssh.ParseAuthorizedKey(line)
	if err != nil {
		return nil, "", false, false
	}
	return key, comment, true, false
}

// log is the store's logger, or silence. See logx.Or.
func (s *Store) log() *slog.Logger {
	return logx.Or(s.Log)
}
