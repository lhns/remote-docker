package accounts

// What the store must keep doing while a sync is in flight. Provisioning shells
// out to useradd, so a sync is seconds long, and Lookup is the SSH public-key
// auth path (agent/internal/sshd/server.go).

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// blockingProvisioner parks inside Ensure until it is released, which is what
// a cold useradd looks like from here. One account only, so Ensure is called
// once.
type blockingProvisioner struct {
	entered chan struct{}
	release chan struct{}
}

func (b *blockingProvisioner) Ensure(name string, _ int, _ string) (string, string, error) {
	close(b.entered)
	<-b.release
	unix := DefaultPrefix + name
	return unix, "/home/" + unix, nil
}

// A sync that is provisioning must not block authentication.
func TestLookupDoesNotWaitForProvisioning(t *testing.T) {
	s := newStore(t)
	s.writeKey(t, "alice.pub")

	prov := &blockingProvisioner{entered: make(chan struct{}), release: make(chan struct{})}
	s.Provisioner = prov

	synced := make(chan error, 1)
	go func() { synced <- s.Sync() }()

	<-prov.entered

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Lookup("alice")
		s.List()
	}()

	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Lookup and List did not return while a provisioner was running; a session cannot authenticate during a sync")
	}

	close(prov.release)
	if err := <-synced; err != nil {
		t.Fatal(err)
	}
}

// Revocation must not edit an *Account that Lookup has already handed out:
// Authorized ranges Keys with no synchronisation. Run under -race.
func TestRevokeDoesNotRaceWithAuthentication(t *testing.T) {
	s := newStore(t)
	key := s.writeKey(t, "alice.pub")
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}

	// Emptied, not removed: that is the revoke path, and it takes two reads.
	s.write(t, "alice.pub", nil)
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	var readers sync.WaitGroup
	for range 8 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if a, ok := s.Lookup("alice"); ok {
					_ = a.Authorized(key)
				}
			}
		}()
	}

	for range 50 {
		if err := s.Sync(); err != nil {
			t.Error(err)
			break
		}
	}
	close(stop)
	readers.Wait()
}

// Two syncs at once must not hand one uid to two accounts. Provisioning is
// outside the write lock now, so nothing but syncMu serialises the allocation.
func TestConcurrentSyncsAllocateDistinctUIDs(t *testing.T) {
	s := newStore(t)
	names := []string{"a", "b", "c", "d", "e", "f"}
	for _, name := range names {
		s.writeKey(t, name+".pub")
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = s.Sync()
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	list := s.List()
	if len(list) != len(names) {
		t.Fatalf("got %d accounts, want %d", len(list), len(names))
	}
	seen := map[int]string{}
	for _, a := range list {
		if other, dup := seen[a.UID]; dup {
			t.Errorf("%s and %s were both given uid %d, so they share a reverse-tunnel port and each other's files", other, a.Name, a.UID)
		}
		seen[a.UID] = a.Name
	}
}

// failingProvisioner starts working and then stops, which is a transient
// useradd failure on a workspace under load.
type failingProvisioner struct {
	fail bool
}

func (f *failingProvisioner) Ensure(name string, _ int, _ string) (string, string, error) {
	if f.fail {
		return "", "", errors.New("useradd: no")
	}
	unix := DefaultPrefix + name
	return unix, "/home/" + unix, nil
}

// A provisioner failing on a later poll must not withdraw access from an
// account that is already enrolled: the symptom is a key that stops working.
func TestFailedProvisioningKeepsAKnownAccount(t *testing.T) {
	s := newStore(t)
	prov := &failingProvisioner{}
	s.Provisioner = prov
	key := s.writeKey(t, "alice.pub")

	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}

	prov.fail = true

	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}

	alice, ok := s.Lookup("alice")
	if !ok {
		t.Fatal("alice was dropped because a later useradd failed")
	}
	if !alice.Authorized(key) {
		t.Error("alice's key stopped authenticating because a later useradd failed")
	}
}
