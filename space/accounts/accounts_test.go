package accounts

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/lhns/remote-docker/pkg/workspace"
)

// fakeProvisioner records what would have been created. Creating real unix
// users needs root, which is exactly why the shell test suite could never run
// in CI, and why this is behind an interface.
type fakeProvisioner struct {
	created map[string]int
	err     error
}

func (f *fakeProvisioner) Ensure(name string, uid int, _ string) (string, string, error) {
	if f.err != nil {
		return "", "", f.err
	}
	if f.created == nil {
		f.created = map[string]int{}
	}
	f.created[name] = uid
	// Prefixed, as the real one does, so a test that confuses the account name
	// with the unix name fails here rather than on a workspace.
	unix := DefaultPrefix + name
	return unix, "/home/" + unix, nil
}

type testStore struct {
	*Store
	keysDir  string
	stateDir string
	prov     *fakeProvisioner
}

func newStore(t *testing.T) *testStore {
	t.Helper()
	root := t.TempDir()
	keys := filepath.Join(root, "keys")
	state := filepath.Join(root, "state")
	if err := os.MkdirAll(keys, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}

	prov := &fakeProvisioner{}
	return &testStore{
		Store:    New(keys, state, workspace.DefaultMapping(), prov, nil),
		keysDir:  keys,
		stateDir: state,
		prov:     prov,
	}
}

// writeKey enrols a generated key as the given filename, returning the key.
func (s *testStore) writeKey(t *testing.T, filename string) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.keysDir, filename)
	if err := os.WriteFile(path, ssh.MarshalAuthorizedKey(key), 0o644); err != nil {
		t.Fatal(err)
	}
	return key
}

func TestSyncProvisionsAnAccountPerKeyFile(t *testing.T) {
	s := newStore(t)
	aliceKey := s.writeKey(t, "alice.pub")
	s.writeKey(t, "bob.pub")

	if err := s.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	alice, ok := s.Lookup("alice")
	if !ok {
		t.Fatal("alice was not provisioned")
	}
	if alice.UID != workspace.DefaultUIDBase {
		t.Errorf("alice uid = %d, want %d", alice.UID, workspace.DefaultUIDBase)
	}
	if !alice.Authorized(aliceKey) {
		t.Error("alice's own key was not accepted")
	}

	bob, ok := s.Lookup("bob")
	if !ok {
		t.Fatal("bob was not provisioned")
	}
	if bob.UID == alice.UID {
		t.Errorf("alice and bob share uid %d", bob.UID)
	}

	// A uid determines the reverse-tunnel port, so it must map cleanly.
	if _, err := workspace.DefaultMapping().PortForUID(alice.UID); err != nil {
		t.Errorf("alice's uid does not map to a port: %v", err)
	}
}

// The bug the shell version had: two files deriving the same account name, the
// second silently overwriting the first's access.
func TestSyncRefusesCollidingNames(t *testing.T) {
	// Deliberately NOT alice.pub and Alice.pub: those are the same file on a
	// case-insensitive filesystem, so this test would silently measure
	// nothing on macOS or Windows. These two differ on every filesystem and
	// still derive the same account name.
	s := newStore(t)
	exact := s.writeKey(t, "alice-smith.pub")
	folded := s.writeKey(t, "alice.smith.pub")

	if err := s.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	alice, ok := s.Lookup("alice-smith")
	if !ok {
		t.Fatal("no account was provisioned at all")
	}

	// The file already named "alice-smith" wins, because that is the spelling
	// the enroller meant. Sorted order alone would be deterministic but
	// arbitrary.
	if !alice.Authorized(exact) {
		t.Error("alice-smith.pub did not claim the account named alice-smith")
	}
	if alice.Authorized(folded) {
		t.Error("a colliding key file was silently merged into the account")
	}
	if got := len(s.List()); got != 1 {
		t.Errorf("provisioned %d accounts from colliding files, want 1", got)
	}
}

// uids are persisted so an account keeps the same reverse-tunnel port and the
// same ownership of everything it has written.
func TestSyncKeepsUIDsAcrossRestarts(t *testing.T) {
	s := newStore(t)
	s.writeKey(t, "alice.pub")
	s.writeKey(t, "bob.pub")
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}

	before := map[string]int{}
	for _, a := range s.List() {
		before[a.Name] = a.UID
	}

	// A fresh store over the same state, as a restarted container would be.
	restarted := New(s.keysDir, s.stateDir, workspace.DefaultMapping(), &fakeProvisioner{}, nil)
	if err := restarted.Sync(); err != nil {
		t.Fatal(err)
	}
	for _, a := range restarted.List() {
		if before[a.Name] != a.UID {
			t.Errorf("%s uid changed from %d to %d across a restart", a.Name, before[a.Name], a.UID)
		}
	}
}

// A new account must not reuse a departed one's uid, or it would inherit the
// files that account left behind.
func TestSyncDoesNotReuseUIDs(t *testing.T) {
	s := newStore(t)
	s.writeKey(t, "alice.pub")
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}
	aliceUID, _ := s.Lookup("alice")

	if err := os.Remove(filepath.Join(s.keysDir, "alice.pub")); err != nil {
		t.Fatal(err)
	}
	s.writeKey(t, "carol.pub")
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}

	carol, ok := s.Lookup("carol")
	if !ok {
		t.Fatal("carol was not provisioned")
	}
	if carol.UID == aliceUID.UID {
		t.Errorf("carol reused alice's uid %d and would inherit her files", carol.UID)
	}
}

// Revoke, do not delete. A key file is removed far more often than a person
// leaves for good, and deleting the home directory is a silent way to lose
// whatever they left there.
func TestSyncRevokesWithoutDeleting(t *testing.T) {
	s := newStore(t)
	key := s.writeKey(t, "alice.pub")
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(filepath.Join(s.keysDir, "alice.pub")); err != nil {
		t.Fatal(err)
	}
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}

	alice, ok := s.Lookup("alice")
	if !ok {
		t.Fatal("the account was removed entirely rather than revoked")
	}
	if alice.Authorized(key) {
		t.Error("a revoked key still authenticates")
	}
	if alice.UID == 0 {
		t.Error("the account lost its uid, and with it its file ownership")
	}
}

// Restoring the key file restores access, at the same uid.
func TestSyncRestoresRevokedAccounts(t *testing.T) {
	s := newStore(t)
	s.writeKey(t, "alice.pub")
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}
	original, _ := s.Lookup("alice")
	uid := original.UID

	if err := os.Remove(filepath.Join(s.keysDir, "alice.pub")); err != nil {
		t.Fatal(err)
	}
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}

	key := s.writeKey(t, "alice.pub")
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}

	alice, _ := s.Lookup("alice")
	if !alice.Authorized(key) {
		t.Error("access was not restored")
	}
	if alice.UID != uid {
		t.Errorf("uid changed from %d to %d on restore", uid, alice.UID)
	}
}

func TestSyncIgnoresUnusableFiles(t *testing.T) {
	s := newStore(t)
	s.writeKey(t, "good.pub")

	for name, contents := range map[string]string{
		"empty.pub":   "",
		"garbage.pub": "this is not a public key\n",
		"notakey.txt": "ssh-ed25519 AAAA...\n", // wrong extension
		"123.pub":     "",                      // no usable name either
	} {
		if err := os.WriteFile(filepath.Join(s.keysDir, name), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	names := []string{}
	for _, a := range s.List() {
		names = append(names, a.Name)
	}
	if !slices.Equal(names, []string{"good"}) {
		t.Errorf("provisioned %v, want [good]", names)
	}
}

func TestAuthenticate(t *testing.T) {
	s := newStore(t)
	aliceKey := s.writeKey(t, "alice.pub")
	bobKey := s.writeKey(t, "bob.pub")
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}

	if !s.Authenticate("alice", aliceKey) {
		t.Error("alice's key was rejected for alice")
	}
	// The one that matters: one account's key must not open another's.
	if s.Authenticate("alice", bobKey) {
		t.Error("bob's key authenticated as alice")
	}
	if s.Authenticate("nobody", aliceKey) {
		t.Error("an unknown account authenticated")
	}
}

func TestSanitizeName(t *testing.T) {
	tests := map[string]string{
		"alice":         "alice",
		"Alice":         "alice",
		"ALICE":         "alice",
		"alice.smith":   "alice-smith",
		"alice@example": "alice-example",
		"alice_smith":   "alice_smith",
		"alice-smith":   "alice-smith",
		"123alice":      "alice",
		"_alice":        "_alice",
	}
	for in, want := range tests {
		got, err := SanitizeName(in)
		if err != nil {
			t.Errorf("SanitizeName(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("SanitizeName(%q) = %q, want %q", in, got, want)
		}
	}

	for _, in := range []string{"", "123", "---", "..."} {
		if got, err := SanitizeName(in); err == nil {
			t.Errorf("SanitizeName(%q) = %q, want an error", in, got)
		}
	}

	long, err := SanitizeName(strings.Repeat("a", 60))
	if err != nil {
		t.Fatal(err)
	}
	if len(long) != maxNameLength {
		t.Errorf("a long name produced %d characters, want %d", len(long), maxNameLength)
	}
}

// The keys directory is expected to live on shared storage where inotify never
// fires for a change made on another host, so polling has to work on its own.
func TestWatchPicksUpChangesByPolling(t *testing.T) {
	s := newStore(t)
	s.writeKey(t, "alice.pub")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- s.Watch(ctx, 50*time.Millisecond) }()

	// The initial sync happens before Watch blocks, so alice is there at once.
	waitFor(t, func() bool { _, ok := s.Lookup("alice"); return ok })

	s.writeKey(t, "bob.pub")
	waitFor(t, func() bool { _, ok := s.Lookup("bob"); return ok })

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Watch: %v", err)
	}
}

// A keys directory that cannot be watched must not stop the agent starting.
func TestWatchSurvivesAnUnwatchableDirectory(t *testing.T) {
	s := newStore(t)
	s.writeKey(t, "alice.pub")
	s.KeysDir = filepath.Join(t.TempDir(), "does-not-exist")

	if err := s.Watch(t.Context(), 50*time.Millisecond); err == nil {
		t.Error("Watch over a missing directory should report the initial sync failure")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition never became true")
}

// uid allocation must not depend on Go's map iteration order.
//
// Sync sorts the key files so a collision resolves deterministically, then
// handed the result to reconcile as a map, which ranged it to ASSIGN uids.
// So which account got which uid, and therefore which reverse-tunnel port,
// differed between runs on a fresh workspace. It showed up as a test that
// failed about one run in eight.
func TestUIDAllocationFollowsSortedNames(t *testing.T) {
	// Names deliberately not in insertion or creation order.
	names := []string{"delta", "alpha", "charlie", "bravo", "echo"}

	for attempt := range 20 {
		s := newStore(t)
		for _, n := range names {
			s.writeKey(t, n+".pub")
		}
		if err := s.Sync(); err != nil {
			t.Fatalf("Sync: %v", err)
		}

		for i, n := range []string{"alpha", "bravo", "charlie", "delta", "echo"} {
			a, ok := s.Lookup(n)
			if !ok {
				t.Fatalf("attempt %d: %s was not provisioned", attempt, n)
			}
			if want := workspace.DefaultUIDBase + i; a.UID != want {
				t.Fatalf("attempt %d: %s uid = %d, want %d -- allocation is not deterministic",
					attempt, n, a.UID, want)
			}
		}
	}
}
