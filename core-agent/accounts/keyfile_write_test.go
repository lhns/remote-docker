package accounts

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func (s *testStore) enrolledFile(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(s.enrolledDir, name+".pub"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestAppendAndRemoveKeepEveryOtherLine(t *testing.T) {
	s := newStore(t)
	first, firstLine := keyLine(t)
	original := "# alice's laptop\nnot a key at all\n" + string(firstLine)
	if err := os.WriteFile(filepath.Join(s.enrolledDir, "alice.pub"), []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	second := newKey(t)
	added, err := s.AppendKey("alice", second, "alice@desktop")
	if err != nil || !added {
		t.Fatalf("AppendKey = %v, %v", added, err)
	}
	got := s.enrolledFile(t, "alice")
	if !strings.HasPrefix(got, original) || !strings.Contains(got, "alice@desktop") {
		t.Fatalf("after the append the file is:\n%s", got)
	}
	if !s.authorized(t, "alice", second) {
		t.Error("the appended key is not in force when AppendKey returns")
	}

	if added, err := s.AppendKey("alice", second, ""); err != nil || added {
		t.Errorf("appending a key already there = %v, %v; want false, nil", added, err)
	}
	if strings.Count(s.enrolledFile(t, "alice"), "ssh-ed25519") != 2 {
		t.Error("a key already in the file was appended again")
	}

	removed, err := s.RemoveKey("alice", ssh.FingerprintSHA256(first))
	if err != nil || !removed {
		t.Fatalf("RemoveKey = %v, %v", removed, err)
	}
	got = s.enrolledFile(t, "alice")
	if !strings.HasPrefix(got, "# alice's laptop\nnot a key at all\n") || strings.Contains(got, string(firstLine)) {
		t.Errorf("after the removal the file is:\n%s", got)
	}
	if s.authorized(t, "alice", first) {
		t.Error("the removed key still authenticates")
	}
}

// A comment is one line whatever it is given: a newline in it would put a
// line of the caller's choosing into the file.
func TestACommentCannotAddALine(t *testing.T) {
	s := newStore(t)
	_, smuggled := keyLine(t)
	if _, err := s.AppendKey("alice", newKey(t), "laptop\n"+string(smuggled)); err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(s.enrolledFile(t, "alice"), "\n"); lines != 1 {
		t.Errorf("the file has %d lines, want 1:\n%s", lines, s.enrolledFile(t, "alice"))
	}
}

// A file with no key left is deleted, so the revocation is immediate rather
// than waiting for the second read of an empty file.
func TestRemovingTheLastKeyDeletesTheFile(t *testing.T) {
	s := newStore(t)
	key := newKey(t)
	if _, err := s.AppendKey("alice", key, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemoveKey("alice", ssh.FingerprintSHA256(key)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.enrolledDir, "alice.pub")); !os.IsNotExist(err) {
		t.Errorf("the emptied file is still there: %v", err)
	}
	if s.authorized(t, "alice", key) {
		t.Error("the account was not revoked at once")
	}
}

func TestConcurrentAppendsLoseNothing(t *testing.T) {
	s := newStore(t)
	keys := make([]ssh.PublicKey, 16)
	for i := range keys {
		keys[i] = newKey(t)
	}

	var wg sync.WaitGroup
	for _, k := range keys {
		wg.Go(func() {
			if _, err := s.AppendKey("alice", k, ""); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()

	s.sync(t)
	for i, k := range keys {
		if !s.authorized(t, "alice", k) {
			t.Errorf("key %d was lost", i)
		}
	}
}

func TestConcurrentCreatesHaveOneWinner(t *testing.T) {
	s := newStore(t)
	var mu sync.Mutex
	var won, lost int
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			err := s.CreateAccount("bob", newKey(t), "")
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				won++
			case errors.Is(err, ErrAccountExists):
				lost++
			default:
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if won != 1 || lost != 7 {
		t.Errorf("%d creations won and %d were refused, want 1 and 7", won, lost)
	}
}

// A name is never handed out twice: one the uidmap has, or one with a file in
// any directory, is taken.
func TestCreateAccountRefusesATakenName(t *testing.T) {
	s := newStore(t)
	writeIn(t, s.keysDir, "alice.pub", newKey(t))
	s.sync(t)
	if err := s.CreateAccount("alice", newKey(t), ""); !errors.Is(err, ErrAccountExists) {
		t.Errorf("an operator-enrolled name: err = %v", err)
	}

	// Removed and revoked, but still in the uidmap.
	if err := os.Remove(filepath.Join(s.keysDir, "alice.pub")); err != nil {
		t.Fatal(err)
	}
	s.sync(t)
	if err := s.CreateAccount("Alice", newKey(t), ""); !errors.Is(err, ErrAccountExists) {
		t.Errorf("a name the uidmap has: err = %v", err)
	}
}

func TestAStaleLockIsBrokenOnce(t *testing.T) {
	s := newStore(t)
	shortLocks(t, lockStale, time.Second)
	lock := filepath.Join(s.enrolledDir, ".alice.pub.lock")
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}

	keys := make([]ssh.PublicKey, 8)
	var wg sync.WaitGroup
	for i := range keys {
		keys[i] = newKey(t)
		wg.Go(func() {
			if _, err := s.AppendKey("alice", keys[i], ""); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()

	s.sync(t)
	for i, k := range keys {
		if !s.authorized(t, "alice", k) {
			t.Errorf("key %d was lost", i)
		}
	}
	entries, err := os.ReadDir(s.enrolledDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "alice.pub" {
			t.Errorf("%s was left behind", e.Name())
		}
	}
}

func TestALiveLockIsNotBroken(t *testing.T) {
	s := newStore(t)
	shortLocks(t, lockStale, 100*time.Millisecond)

	lock := filepath.Join(s.enrolledDir, ".alice.pub.lock")
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendKey("alice", newKey(t), ""); err == nil || !strings.Contains(err.Error(), "held by another writer") {
		t.Errorf("AppendKey under a live lock: err = %v", err)
	}
	if _, err := os.Stat(lock); err != nil {
		t.Errorf("the live lock was taken away: %v", err)
	}
}

// A queue of writers outlasting lockWait is waited out: the limit is on one
// holder. A new mtime is how a waiter sees the lock change hands.
func TestAMovingQueueIsWaitedOut(t *testing.T) {
	s := newStore(t)
	shortLocks(t, lockStale, 100*time.Millisecond)

	lock := filepath.Join(s.enrolledDir, ".alice.pub.lock")
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.AppendKey("alice", newKey(t), "")
		done <- err
	}()

	for i := range 10 { // 500ms of holders, five times lockWait
		time.Sleep(50 * time.Millisecond)
		select {
		case err := <-done:
			t.Fatalf("AppendKey returned while the lock was held: %v", err)
		default:
		}
		next := time.Now().Add(time.Duration(i+1) * time.Second)
		if err := os.Chtimes(lock, next, next); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Errorf("AppendKey behind a moving queue: %v", err)
	}
}

// Removing only the enrolled copy of a key the operator also enrols would leave
// it working, so the change is refused, naming the operator's file.
func TestAnOperatorKeyIsNotRemovedHere(t *testing.T) {
	s := newStore(t)
	key := newKey(t)
	writeIn(t, s.keysDir, "alice.pub", key)
	writeIn(t, s.enrolledDir, "alice.pub", key)
	s.sync(t)

	operatorFile := filepath.Join(s.keysDir, "alice.pub")
	var refused *OperatorKeyError

	_, err := s.RemoveKey("alice", ssh.FingerprintSHA256(key))
	if !errors.As(err, &refused) || refused.File != operatorFile {
		t.Errorf("RemoveKey: err = %v, want a refusal naming %s", err, operatorFile)
	}
	if !strings.Contains(s.enrolledFile(t, "alice"), "ssh-ed25519") {
		t.Error("the refused removal changed the enrolled file")
	}

	if err := s.RemoveAccountFile("alice"); !errors.As(err, &refused) || refused.File != operatorFile {
		t.Errorf("RemoveAccountFile: err = %v, want a refusal naming %s", err, operatorFile)
	}
	if !s.authorized(t, "alice", key) {
		t.Error("a refused change revoked the key")
	}
}

func TestRemoveAccountFileRevokesAtOnce(t *testing.T) {
	s := newStore(t)
	key := newKey(t)
	if err := s.CreateAccount("bob", key, ""); err != nil {
		t.Fatal(err)
	}
	if !s.authorized(t, "bob", key) {
		t.Fatal("the created account does not authenticate")
	}
	if err := s.RemoveAccountFile("bob"); err != nil {
		t.Fatal(err)
	}
	if s.authorized(t, "bob", key) {
		t.Error("bob still authenticates")
	}
}

func TestNoEnrolledDirectoryRefusesEveryWrite(t *testing.T) {
	s := newStore(t)
	s.EnrolledDir = ""
	if _, err := s.AppendKey("alice", newKey(t), ""); err == nil {
		t.Error("AppendKey wrote with no enrolled directory")
	}
	if err := s.CreateAccount("alice", newKey(t), ""); err == nil {
		t.Error("CreateAccount wrote with no enrolled directory")
	}
}

// A write for an account whose useradd failed must say so and leave nothing
// behind: a nil error means the key is in force.
func TestAWriteForAnAccountThatCannotBeCreatedFails(t *testing.T) {
	s := newStore(t)
	s.prov.err = errors.New("useradd: no")

	if err := s.CreateAccount("bob", newKey(t), ""); !errors.Is(err, ErrNotProvisioned) {
		t.Errorf("CreateAccount: err = %v, want ErrNotProvisioned", err)
	}
	if _, err := os.Stat(filepath.Join(s.enrolledDir, "bob.pub")); !os.IsNotExist(err) {
		t.Errorf("bob.pub was left behind: %v", err)
	}

	if _, err := s.AppendKey("carol", newKey(t), ""); !errors.Is(err, ErrNotProvisioned) {
		t.Errorf("AppendKey: err = %v, want ErrNotProvisioned", err)
	}
	if _, err := os.Stat(filepath.Join(s.enrolledDir, "carol.pub")); !os.IsNotExist(err) {
		t.Errorf("carol.pub was left behind: %v", err)
	}
}

// shortLocks shortens the lock timings for one test.
func shortLocks(t *testing.T, stale, wait time.Duration) {
	t.Helper()
	oldStale, oldWait := lockStale, lockWait
	lockStale, lockWait = stale, wait
	t.Cleanup(func() { lockStale, lockWait = oldStale, oldWait })
}

// A lock's mtime is the filesystem's clock and time.Since is this host's, so
// on shared storage a host whose clock is ahead sees a live lock as old. It
// must still wait for the holder rather than break it.
func TestALiveLockThatLooksOldIsNotBroken(t *testing.T) {
	s := newStore(t)
	shortLocks(t, 50*time.Millisecond, time.Second)

	lock := filepath.Join(s.enrolledDir, ".alice.pub.lock")
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	skewed := time.Now().Add(-time.Hour)
	if err := os.Chtimes(lock, skewed, skewed); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.AppendKey("alice", newKey(t), "")
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("AppendKey returned while the lock was held: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := os.Remove(lock); err != nil {
		t.Fatalf("the holder's lock was taken away: %v", err)
	}
	if err := <-done; err != nil {
		t.Errorf("AppendKey after the holder let go: %v", err)
	}
}

// A stale lock that cannot be broken is given up on after lockWait like any
// other, never spun on.
func TestAStaleLockThatCannotBeBrokenTimesOut(t *testing.T) {
	s := newStore(t)
	shortLocks(t, 50*time.Millisecond, 200*time.Millisecond)

	lock := filepath.Join(s.enrolledDir, ".alice.pub.lock")
	old, future := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	for path, at := range map[string]time.Time{lock: old, lock + ".break": future} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}

	done := make(chan error, 1)
	go func() {
		_, err := s.AppendKey("alice", newKey(t), "")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("AppendKey took a lock that was never released")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("AppendKey is still trying to break a lock it cannot break")
	}
}
