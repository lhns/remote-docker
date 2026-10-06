package sshd

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/lhns/remote-docker/core/enrol"
)

// A redeem and account management while the workspace is creating some other
// account, whose useradd has taken 170s (PR 268).

// gatedProvisioner parks inside Ensure for one account until release closes,
// and provisions every other at once.
type gatedProvisioner struct {
	gated   string
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (g *gatedProvisioner) Ensure(name string, _ int, _ string) (string, string, error) {
	if name == g.gated {
		g.once.Do(func() { close(g.entered) })
		<-g.release
	}
	return name, "/home/" + name, nil
}

func (*gatedProvisioner) Remove(string, int) error { return nil }

// provisioning starts a sync creating account, and returns once its useradd
// is under way. It finishes after hold, so a test that fails still ends.
func (w *tokenWorkspace) provisioning(t *testing.T, account string, hold time.Duration) {
	t.Helper()
	prov := &gatedProvisioner{gated: account, entered: make(chan struct{}), release: make(chan struct{})}
	w.store.Provisioner = prov
	key := newSigner(t)
	if err := os.WriteFile(filepath.Join(w.keysDir, account+".pub"), ssh.MarshalAuthorizedKey(key.PublicKey()), 0o600); err != nil {
		t.Fatal(err)
	}
	go func() { _ = w.store.Sync() }()
	<-prov.entered
	time.AfterFunc(hold, func() { close(prov.release) })
}

// within fails the test if fn took longer than limit.
func within(t *testing.T, limit time.Duration, what string, fn func()) {
	t.Helper()
	start := time.Now()
	fn()
	if took := time.Since(start); took > limit {
		t.Errorf("%s took %v while another account was being created", what, took.Round(time.Millisecond))
	}
}

func TestARedeemAndAKeyAddDoNotWaitForAnotherAccountsProvisioning(t *testing.T) {
	w := startManageWorkspace(t, "alice")
	aliceKey := newSigner(t)
	w.enrol(t, "alice", aliceKey)
	w.provisioning(t, "carol", 15*time.Second)

	// A bound token into an account that exists.
	id, secret := w.mint(t, "alice")
	second := newSigner(t)
	c := w.redeemer(t, id, second)
	var reply enrol.RedeemReply
	within(t, 2*time.Second, "a redeem into alice", func() {
		reply = redeemOn(t, c, enrol.RedeemRequest{Secret: secret})
	})
	if reply.Error != nil || reply.Account != "alice" || reply.Created {
		t.Fatalf("reply %+v, error %+v", reply, reply.Error)
	}
	w.dial(t, "alice", second)

	// An unbound token asks Known.
	id, secret = w.mint(t, "")
	c = w.redeemer(t, id, newSigner(t))
	within(t, 2*time.Second, "refusing an unbound token a taken name", func() {
		wantCode(t, redeemOn(t, c, enrol.RedeemRequest{Secret: secret, Account: "alice"}), enrol.CodeName)
	})

	// Account management on an account that exists.
	third := newSigner(t)
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(third.PublicKey())))
	ca := w.dial(t, "alice", aliceKey)
	within(t, 2*time.Second, "key add for alice", func() {
		wantOK(t, manageOn(t, ca, enrol.Request{Op: enrol.OpKeyAdd, Key: line}))
	})
	w.dial(t, "alice", third)
}

// A redeem that creates an account waits for its useradd only so long, then
// says the account is still being created, and the key works once it is.
func TestARedeemReportsAnAccountStillBeingCreated(t *testing.T) {
	w := startTokenWorkspace(t, "")
	w.store.ProvisionWait = 200 * time.Millisecond
	w.provisioning(t, "carol", 4*time.Second) // one useradd at a time: dave's waits for it

	id, secret := w.mint(t, "")
	key := newSigner(t)
	c := w.redeemer(t, id, key)
	var reply enrol.RedeemReply
	within(t, 2*time.Second, "a redeem creating dave", func() {
		reply = redeemOn(t, c, enrol.RedeemRequest{Secret: secret, Account: "dave"})
	})
	if reply.Error != nil || reply.Account != "dave" || !reply.Created || !reply.Pending {
		t.Fatalf("reply %+v, error %+v; want dave, created and pending", reply, reply.Error)
	}
	if w.tokens.Live(id) {
		t.Error("the token is still live, though its key is enrolled")
	}

	deadline := time.Now().Add(8 * time.Second)
	for {
		if a, ok := w.store.Lookup("dave"); ok && a.Authorized(key.PublicKey()) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("dave never authenticated once its provisioning finished")
		}
		time.Sleep(20 * time.Millisecond)
	}
	w.dial(t, "dave", key)
}

// A key added to an account the workspace is creating again is enrolled, and
// the reply says so rather than reporting a failed write.
func TestAKeyAddReportsAnAccountStillBeingCreated(t *testing.T) {
	w := startManageWorkspace(t, "alice")
	alice := newSigner(t)
	w.enrol(t, "alice", alice)
	w.enrolKey(t, "bob", newSigner(t))
	ca := w.dial(t, "alice", alice)
	wantOK(t, manageOn(t, ca, enrol.Request{Op: enrol.OpUserRemove, Account: "bob", Purge: true, Force: true}))

	// bob is known but no longer provisioned, so a key creates him again.
	prov := &gatedProvisioner{gated: "bob", entered: make(chan struct{}), release: make(chan struct{})}
	defer close(prov.release)
	w.store.Provisioner = prov
	w.store.ProvisionWait = 200 * time.Millisecond

	key := newSigner(t)
	r := manageOn(t, ca, enrol.Request{Op: enrol.OpKeyAdd, Account: "bob", Key: string(ssh.MarshalAuthorizedKey(key.PublicKey()))})
	wantOK(t, r)
	if len(r.Notices) != 1 || !strings.Contains(r.Notices[0].Msg, "still being created") {
		t.Errorf("notices %+v, want one saying bob is still being created", r.Notices)
	}
}
