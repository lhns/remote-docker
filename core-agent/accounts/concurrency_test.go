package accounts

// What the store must keep doing while a sync is in flight. Provisioning shells
// out to useradd, so a sync is seconds long, and Lookup is the SSH public-key
// auth path (agent/internal/sshd/server.go).

import (
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// A sync that is provisioning must not block authentication.
func TestLookupDoesNotWaitForProvisioning(t *testing.T) {
	s := newStore(t)
	s.writeKey(t, "alice.pub")

	prov := newGatedProvisioner("alice")
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

func (*failingProvisioner) Remove(string, int) error { return nil }

// A provisioner failing on a later poll must not withdraw access from an
// account that is already enrolled: the symptom is a key that stops working.
// It holds because Ensure runs once per account per process.
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

// What is carried forward is the account, not its old keys: a key replaced in
// the file is withdrawn even while provisioning fails, which is when somebody
// rotating a leaked key most needs it to be.
func TestFailedProvisioningStillTakesTheFilesKeys(t *testing.T) {
	s := newStore(t)
	prov := &failingProvisioner{}
	s.Provisioner = prov
	old := s.writeKey(t, "alice.pub")
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}

	fresh := s.writeKey(t, "alice.pub")
	prov.fail = true
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}

	alice, ok := s.Lookup("alice")
	if !ok {
		t.Fatal("alice was dropped because a later useradd failed")
	}
	if alice.Authorized(old) {
		t.Error("a key removed from alice.pub still authenticates because a later useradd failed")
	}
	if !alice.Authorized(fresh) {
		t.Error("the key now in alice.pub does not authenticate because a later useradd failed")
	}
}

// gatedProvisioner parks inside Ensure for one account until released, and
// provisions every other at once: one useradd copying a big /etc/skel.
type gatedProvisioner struct {
	gated   string
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func newGatedProvisioner(gated string) *gatedProvisioner {
	return &gatedProvisioner{gated: gated, entered: make(chan struct{}), release: make(chan struct{})}
}

func (g *gatedProvisioner) Ensure(name string, _ int, _ string) (string, string, error) {
	if name == g.gated {
		g.once.Do(func() { close(g.entered) })
		<-g.release
	}
	unix := DefaultPrefix + name
	return unix, "/home/" + unix, nil
}

func (*gatedProvisioner) Remove(string, int) error { return nil }

// prompt fails the test if fn has not returned within a second: the gated
// provisioning takes two, and a key write on a loaded Windows runner more
// than 100ms.
func prompt(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Errorf("%s waited for another account's provisioning", what)
		<-done
	}
}

// A redeem asks Known, and account management writes keys, while a sync may be
// creating somebody else; one useradd has taken 170s (PR 268).
func TestKnownAndKeyWritesDoNotWaitForProvisioning(t *testing.T) {
	s := newStore(t)
	prov := newGatedProvisioner("a")
	s.Provisioner = prov
	s.writeKey(t, "b.pub")
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}

	s.writeKey(t, "a.pub")
	synced := make(chan error, 1)
	go func() { synced <- s.Sync() }()
	<-prov.entered

	// A goroutine stands in for the 170s, so a failure below still ends.
	go func() {
		time.Sleep(2 * time.Second)
		close(prov.release)
	}()

	prompt(t, "Known", func() {
		if known, err := s.Known("c"); err != nil || known {
			t.Errorf("Known(c) = %v, %v; want false", known, err)
		}
		if known, err := s.Known("b"); err != nil || !known {
			t.Errorf("Known(b) = %v, %v; want true", known, err)
		}
	})

	fresh := newKey(t)
	prompt(t, "AppendKey for an account already provisioned", func() {
		if _, err := s.AppendKey("b", fresh, ""); err != nil {
			t.Error(err)
		}
	})
	if b, ok := s.Lookup("b"); !ok || !b.Authorized(fresh) {
		t.Error("the key added to b does not authenticate once AppendKey returns")
	}

	prompt(t, "RemoveKey", func() {
		if _, err := s.RemoveKey("b", ssh.FingerprintSHA256(fresh)); err != nil {
			t.Error(err)
		}
	})
	if b, _ := s.Lookup("b"); b.Authorized(fresh) {
		t.Error("the key removed from b still authenticates once RemoveKey returns")
	}

	if err := <-synced; err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Lookup("a"); !ok {
		t.Error("a was not published once its provisioning finished")
	}
}

// Known reads the uidmap that syncs write. Run under -race.
func TestKnownAgainstConcurrentSyncs(t *testing.T) {
	s := newStore(t)
	stop := make(chan struct{})
	var readers sync.WaitGroup
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := s.Known("n3"); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}

	var syncs sync.WaitGroup
	for i := range 8 {
		s.writeKey(t, "n"+strconv.Itoa(i)+".pub")
		syncs.Add(1)
		go func() {
			defer syncs.Done()
			if err := s.Sync(); err != nil {
				t.Error(err)
			}
		}()
	}
	syncs.Wait()
	close(stop)
	readers.Wait()

	for i := range 8 {
		if known, err := s.Known("n" + strconv.Itoa(i)); err != nil || !known {
			t.Errorf("Known(n%d) = %v, %v after its sync", i, known, err)
		}
	}
}
