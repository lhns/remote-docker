package accounts

// Several keys directories merged by account (ADR 0052): the operator's, which
// are only read, and the enrolled one, which the agent writes.

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/lhns/remote-docker/core/workspace"
)

// writeIn replaces a key file in dir.
func writeIn(t *testing.T, dir, filename string, keys ...ssh.PublicKey) {
	t.Helper()
	var body []byte
	for _, k := range keys {
		body = append(body, ssh.MarshalAuthorizedKey(k)...)
	}
	if err := os.WriteFile(filepath.Join(dir, filename), body, 0o644); err != nil {
		t.Fatal(err)
	}
}

func newKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	k, _ := keyLine(t)
	return k
}

func (s *testStore) sync(t *testing.T) {
	t.Helper()
	if err := s.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
}

func TestKeysAreMergedAcrossDirectoriesByAccount(t *testing.T) {
	s := newStore(t)
	k1, k2, k3 := newKey(t), newKey(t), newKey(t)
	writeIn(t, s.keysDir, "alice.pub", k1, k2)
	writeIn(t, s.enrolledDir, "Alice.pub", k2, k3) // folds to alice too
	s.sync(t)

	a, ok := s.Lookup("alice")
	if !ok {
		t.Fatal("alice was not provisioned")
	}
	if len(a.Keys) != 3 {
		t.Errorf("alice has %d keys, want 3: the one in both directories counts once", len(a.Keys))
	}
	for i, k := range []ssh.PublicKey{k1, k2, k3} {
		if !a.Authorized(k) {
			t.Errorf("key %d is not accepted", i+1)
		}
	}
	if len(a.Sources) != 2 || a.Sources[0].Dir != s.keysDir || a.Sources[1].Dir != s.enrolledDir {
		t.Fatalf("sources = %+v, want the operator directory then the enrolled one", a.Sources)
	}
	if len(a.Sources[1].Keys) != 2 || len(a.Sources[1].Comments) != 2 {
		t.Errorf("the enrolled source has %d keys and %d comments, want 2 and 2",
			len(a.Sources[1].Keys), len(a.Sources[1].Comments))
	}
}

// The two-read rule is per directory: an emptied file keeps contributing its
// keys for one read, and the other directory's keys are never at stake.
func TestAnEmptiedFileCarriesItsKeysForwardOnceInItsOwnDirectory(t *testing.T) {
	s := newStore(t)
	operator, enrolled := newKey(t), newKey(t)
	writeIn(t, s.keysDir, "alice.pub", operator)
	writeIn(t, s.enrolledDir, "alice.pub", enrolled)
	s.sync(t)

	writeIn(t, s.enrolledDir, "alice.pub") // caught mid-save
	s.sync(t)
	if !s.authorized(t, "alice", enrolled) {
		t.Fatal("the first read of an empty file revoked its key")
	}

	s.sync(t)
	if s.authorized(t, "alice", enrolled) {
		t.Error("the second read of an empty file did not revoke its key")
	}
	if !s.authorized(t, "alice", operator) {
		t.Error("emptying the enrolled file revoked the operator's key")
	}
}

func TestAMissingFileRevokesItsKeysAtOnce(t *testing.T) {
	s := newStore(t)
	operator, enrolled := newKey(t), newKey(t)
	writeIn(t, s.keysDir, "alice.pub", operator)
	writeIn(t, s.enrolledDir, "alice.pub", enrolled)
	s.sync(t)

	if err := os.Remove(filepath.Join(s.enrolledDir, "alice.pub")); err != nil {
		t.Fatal(err)
	}
	s.sync(t)
	if s.authorized(t, "alice", enrolled) {
		t.Error("a deleted file's key still authenticates")
	}
	if !s.authorized(t, "alice", operator) {
		t.Error("deleting the enrolled file revoked the operator's key")
	}

	if err := os.Remove(filepath.Join(s.keysDir, "alice.pub")); err != nil {
		t.Fatal(err)
	}
	s.sync(t)
	if a, _ := s.Lookup("alice"); len(a.Keys) != 0 {
		t.Error("alice is still enrolled with no file in any directory")
	}
}

// The agent serves without the enrolled directory, and the operator's keys
// still work.
func TestAMissingEnrolledDirectoryIsEmpty(t *testing.T) {
	s := newStore(t)
	key := s.writeKey(t, "alice.pub")
	s.EnrolledDir = filepath.Join(t.TempDir(), "never-created")
	s.sync(t)
	if !s.authorized(t, "alice", key) {
		t.Error("alice's operator key was not accepted")
	}
}

func TestSeveralOperatorDirectoriesAreRead(t *testing.T) {
	s := newStore(t)
	second := t.TempDir()
	s.KeysDirs = append(s.KeysDirs, second)
	alice, bob := newKey(t), newKey(t)
	writeIn(t, s.keysDir, "alice.pub", alice)
	writeIn(t, second, "bob.pub", bob)
	s.sync(t)
	if !s.authorized(t, "alice", alice) || !s.authorized(t, "bob", bob) {
		t.Error("a key in one of the operator directories was not accepted")
	}
}

// The writer's locks and temporary files are dotfiles, and enrol nobody.
func TestDotfilesEnrolNobody(t *testing.T) {
	s := newStore(t)
	writeIn(t, s.enrolledDir, ".alice.pub", newKey(t))
	s.sync(t)
	if _, ok := s.Lookup("alice"); ok {
		t.Error("a dotfile enrolled an account")
	}
}

// uids follow sorted names wherever the files are.
func TestUIDsFollowSortedNamesAcrossDirectories(t *testing.T) {
	for attempt := range 10 {
		s := newStore(t)
		writeIn(t, s.enrolledDir, "alpha.pub", newKey(t))
		writeIn(t, s.keysDir, "delta.pub", newKey(t))
		writeIn(t, s.enrolledDir, "charlie.pub", newKey(t))
		writeIn(t, s.keysDir, "bravo.pub", newKey(t))
		s.sync(t)
		for i, n := range []string{"alpha", "bravo", "charlie", "delta"} {
			a, ok := s.Lookup(n)
			if !ok {
				t.Fatalf("%s was not provisioned", n)
			}
			if want := workspace.DefaultUIDBase + i; a.UID != want {
				t.Fatalf("attempt %d: %s uid = %d, want %d", attempt, n, a.UID, want)
			}
		}
	}
}

func TestWatchSeesTheEnrolledDirectory(t *testing.T) {
	s := newStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Watch(ctx, 50*time.Millisecond) }()

	key := newKey(t)
	writeIn(t, s.enrolledDir, "bob.pub", key)
	waitFor(t, func() bool { a, ok := s.Lookup("bob"); return ok && a.Authorized(key) })

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Watch: %v", err)
	}
}

func TestCheckDirs(t *testing.T) {
	root := t.TempDir()
	keys := filepath.Join(root, "keys")
	other := filepath.Join(root, "other")
	for _, d := range []string{keys, other} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	refused := []struct {
		name     string
		operator []string
		enrolled string
		want     string
	}{
		{"the same directory", []string{keys}, keys + string(filepath.Separator), "also an operator"},
		{"inside", []string{keys}, filepath.Join(keys, "enrolled"), "is inside"},
		{"containing", []string{keys}, root, "contains"},
		{"named twice", []string{keys, filepath.Join(other, "..", "keys")}, filepath.Join(root, "enrolled"), "named twice"},
	}
	for _, c := range refused {
		err := CheckDirs(c.operator, c.enrolled)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want one saying %q", c.name, err, c.want)
		}
	}

	allowed := []struct {
		name     string
		operator []string
		enrolled string
	}{
		{"siblings", []string{keys, other}, filepath.Join(root, "enrolled")},
		{"a shared prefix is not containment", []string{keys}, keys + "2"},
		{"no enrolled directory", []string{keys}, ""},
	}
	for _, c := range allowed {
		if err := CheckDirs(c.operator, c.enrolled); err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
	}

	link := filepath.Join(root, "link")
	if err := os.Symlink(keys, link); err != nil {
		t.Logf("no symlink here (%v); skipping the symlinked case", err)
		return
	}
	if err := CheckDirs([]string{keys}, filepath.Join(link, "enrolled")); err == nil {
		t.Error("an enrolled directory inside the operator's through a symlink was accepted")
	}
}

func TestCheckWritable(t *testing.T) {
	s := newStore(t)
	if err := s.CheckWritable(); err != nil {
		t.Errorf("a writable directory: %v", err)
	}
	if entries, _ := os.ReadDir(s.enrolledDir); len(entries) != 0 {
		t.Errorf("the probe left %d entries behind", len(entries))
	}

	s.EnrolledDir = filepath.Join(t.TempDir(), "missing")
	if err := s.CheckWritable(); err == nil {
		t.Error("a missing directory was called writable")
	}
	s.EnrolledDir = ""
	if err := s.CheckWritable(); err == nil {
		t.Error("no directory was called writable")
	}
}

func TestAReadOnlyEnrolledDirectoryRefusesWrites(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a directory this process cannot write")
	}
	s := newStore(t)
	if err := os.Chmod(s.enrolledDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(s.enrolledDir, 0o755) })

	if err := s.CheckWritable(); err == nil {
		t.Error("a read-only directory was called writable")
	}
	if _, err := s.AppendKey("alice", newKey(t), ""); err == nil {
		t.Error("a key was appended in a read-only directory")
	}
}
