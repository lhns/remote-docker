package sshd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/lhns/remote-docker/agent/internal/daemons"
	"github.com/lhns/remote-docker/agent/internal/ephemeral"
	"github.com/lhns/remote-docker/core-agent/accounts"
	"github.com/lhns/remote-docker/core/enrol"
	"github.com/lhns/remote-docker/core/workspace"
)

// Account management (ADR 0053), one operation per request, over real SSH
// connections. authorize's whole table is enrolpolicy_test.go; these are what
// each operation does with its answer.

type manageWorkspace struct {
	*tokenWorkspace
	targets *fakeTargets
	ports   *accounts.Ports
	runs    *ephemeral.Registry
}

func startManageWorkspace(t *testing.T, admins ...string) *manageWorkspace {
	t.Helper()
	targets := &fakeTargets{byAccount: map[string]daemons.Target{}, running: map[string]int{}}
	ports := &accounts.Ports{Dir: t.TempDir(), Mapping: workspace.DefaultMapping()}
	runs := &ephemeral.Registry{Ports: ports}
	set := map[string]bool{}
	for _, a := range admins {
		set[a] = true
	}
	w := startTokenWorkspace(t, "", func(c *Config) {
		c.Admins = set
		c.Daemons = targets
		c.Ports = ports
		c.Runs = runs
	})
	runs.Enrolled = Enrolled(w.store) // as serve wires it with a daemon per account
	return &manageWorkspace{tokenWorkspace: w, targets: targets, ports: ports, runs: runs}
}

// enrolKey writes key into the enrolled directory, as a token would.
func (w *manageWorkspace) enrolKey(t *testing.T, account string, key ssh.Signer) {
	t.Helper()
	if _, err := w.store.AppendKey(account, key.PublicKey(), account+"@test"); err != nil {
		t.Fatal(err)
	}
}

func manageOn(t *testing.T, c *ssh.Client, req enrol.Request) enrol.Reply {
	t.Helper()
	s, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	body, _ := json.Marshal(req)
	s.Stdin = bytes.NewReader(body)
	out, err := s.Output(enrol.EnrolCommand)
	if err != nil {
		t.Fatalf("%s: %v", enrol.EnrolCommand, err)
	}
	var reply enrol.Reply
	if err := json.Unmarshal(out, &reply); err != nil {
		t.Fatalf("reply %q: %v", out, err)
	}
	return reply
}

func wantOK(t *testing.T, r enrol.Reply) {
	t.Helper()
	if r.Error != nil {
		t.Fatalf("refused: %v", r.Error)
	}
}

func wantRefused(t *testing.T, r enrol.Reply, code, msg string) {
	t.Helper()
	if r.Error == nil || r.Error.Code != code || !strings.Contains(r.Error.Msg, msg) {
		t.Fatalf("got %+v, want %s %q", r.Error, code, msg)
	}
}

func TestWhoamiIsTheConnection(t *testing.T) {
	w := startManageWorkspace(t, "alice")
	key := newSigner(t)
	w.enrol(t, "alice", key)

	r := manageOn(t, w.dial(t, "Alice", key), enrol.Request{Op: enrol.OpWhoami})
	wantOK(t, r)
	if r.Whoami == nil || r.Whoami.Account != "alice" || !r.Whoami.Admin || r.Whoami.Key != ssh.FingerprintSHA256(key.PublicKey()) {
		t.Fatalf("whoami %+v", r.Whoami)
	}
}

func TestANonAdminMintsOnlyForThemselves(t *testing.T) {
	w := startManageWorkspace(t, "alice")
	alice, bob := newSigner(t), newSigner(t)
	w.enrol(t, "alice", alice)
	w.enrol(t, "bob", bob)
	ca, cb := w.dial(t, "alice", alice), w.dial(t, "bob", bob)

	r := manageOn(t, cb, enrol.Request{Op: enrol.OpTokenCreate, Note: "phone"})
	wantOK(t, r)
	if r.Token == nil || r.Token.Account != "bob" || r.Token.Creator != "bob" || r.Token.Note != "phone" {
		t.Fatalf("token %+v", r.Token)
	}
	own, secret, err := enrol.ParseToken(r.Token.Token)
	if err != nil || own != r.Token.ID {
		t.Fatalf("token %q: %v", r.Token.Token, err)
	}

	wantRefused(t, manageOn(t, cb, enrol.Request{Op: enrol.OpTokenCreate, Account: "carol"}), enrol.CodeDenied,
		"only an admin can create a token for another account (you are bob)")
	wantRefused(t, manageOn(t, cb, enrol.Request{Op: enrol.OpTokenCreate, Unbound: true}), enrol.CodeDenied,
		"for a new account")

	// An admin's token for carol: bob sees only his own, and cannot remove it.
	r = manageOn(t, ca, enrol.Request{Op: enrol.OpTokenCreate, Account: "carol"})
	wantOK(t, r)
	carols := r.Token.ID
	if r := manageOn(t, cb, enrol.Request{Op: enrol.OpTokenList}); len(r.Tokens) != 1 || r.Tokens[0].ID != own {
		t.Errorf("bob lists %+v, want only his own", r.Tokens)
	}
	if r := manageOn(t, ca, enrol.Request{Op: enrol.OpTokenList}); len(r.Tokens) != 2 {
		t.Errorf("an admin lists %+v, want both", r.Tokens)
	}
	wantRefused(t, manageOn(t, cb, enrol.Request{Op: enrol.OpTokenRemove, ID: carols}), enrol.CodeUnknown, "no token "+carols)
	if !w.tokens.Live(carols) {
		t.Error("a refused removal removed the token")
	}

	// bob's own token enrols a second machine into bob.
	phone := newSigner(t)
	reply := w.redeem(t, own, phone, enrol.RedeemRequest{Secret: secret})
	if reply.Error != nil || reply.Account != "bob" || reply.Created {
		t.Fatalf("redeeming bob's own token: %+v, %+v", reply, reply.Error)
	}
	w.dial(t, "bob", phone)
}

func TestTokenQuotaCapsANonAdminOnly(t *testing.T) {
	w := startManageWorkspace(t, "alice")
	alice, bob := newSigner(t), newSigner(t)
	w.enrol(t, "alice", alice)
	w.enrol(t, "bob", bob)
	ca, cb := w.dial(t, "alice", alice), w.dial(t, "bob", bob)
	create := enrol.Request{Op: enrol.OpTokenCreate}

	// Expired tokens do not count.
	past := time.Now().Add(-48 * time.Hour)
	w.tokens.Now = func() time.Time { return past }
	for i := 0; i < maxSelfTokens; i++ {
		if _, _, err := w.tokens.Mint("bob", "bob", "", time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	w.tokens.Now = nil

	var first string
	for i := 0; i < maxSelfTokens; i++ {
		r := manageOn(t, cb, create)
		wantOK(t, r)
		if i == 0 {
			first = r.Token.ID
		}
	}
	wantRefused(t, manageOn(t, cb, create), enrol.CodeDenied, "you already have 10 unused tokens")

	// An admin is not capped, and does not count against bob.
	for i := 0; i < maxSelfTokens+1; i++ {
		wantOK(t, manageOn(t, ca, create))
	}

	// Removing one frees a slot.
	wantOK(t, manageOn(t, cb, enrol.Request{Op: enrol.OpTokenRemove, ID: first}))
	wantOK(t, manageOn(t, cb, create))
	wantRefused(t, manageOn(t, cb, create), enrol.CodeDenied, "you already have")
}

func TestARedeemedTokenIsNotCounted(t *testing.T) {
	w := startManageWorkspace(t)
	bob := newSigner(t)
	w.enrol(t, "bob", bob)
	cb := w.dial(t, "bob", bob)
	create := enrol.Request{Op: enrol.OpTokenCreate}
	for i := 0; i < maxSelfTokens-1; i++ {
		wantOK(t, manageOn(t, cb, create))
	}
	r := manageOn(t, cb, create)
	wantOK(t, r)
	wantRefused(t, manageOn(t, cb, create), enrol.CodeDenied, "you already have 10 unused tokens")

	id, secret, err := enrol.ParseToken(r.Token.Token)
	if err != nil {
		t.Fatal(err)
	}
	if reply := w.redeem(t, id, newSigner(t), enrol.RedeemRequest{Secret: secret}); reply.Error != nil {
		t.Fatal(reply.Error)
	}
	wantOK(t, manageOn(t, cb, create))
}

func TestTokenRemove(t *testing.T) {
	w := startManageWorkspace(t)
	bob := newSigner(t)
	w.enrol(t, "bob", bob)
	cb := w.dial(t, "bob", bob)

	r := manageOn(t, cb, enrol.Request{Op: enrol.OpTokenCreate})
	wantOK(t, r)
	wantOK(t, manageOn(t, cb, enrol.Request{Op: enrol.OpTokenRemove, ID: r.Token.ID}))
	if w.tokens.Live(r.Token.ID) {
		t.Error("the token is still live")
	}
	wantRefused(t, manageOn(t, cb, enrol.Request{Op: enrol.OpTokenRemove, ID: r.Token.ID}), enrol.CodeUnknown, "no token")
}

// A token the caller may not remove is one they cannot see, so the reply is
// the same whether it exists or not.
func TestTokenRemoveIsNoOracle(t *testing.T) {
	w := startManageWorkspace(t, "alice")
	bob := newSigner(t)
	w.enrol(t, "bob", bob)
	cb := w.dial(t, "bob", bob)
	carols, _ := w.mint(t, "carol")
	gone, _ := w.mint(t, "carol")
	if err := w.tokens.Revoke(gone); err != nil {
		t.Fatal(err)
	}

	exists := manageOn(t, cb, enrol.Request{Op: enrol.OpTokenRemove, ID: carols})
	missing := manageOn(t, cb, enrol.Request{Op: enrol.OpTokenRemove, ID: gone})
	if exists.Error == nil || missing.Error == nil {
		t.Fatalf("refusals %+v, %+v", exists.Error, missing.Error)
	}
	if exists.Error.Code != missing.Error.Code || strings.ReplaceAll(exists.Error.Msg, carols, gone) != missing.Error.Msg {
		t.Errorf("carol's token: %+v; a removed one: %+v; want the same reply", exists.Error, missing.Error)
	}
	if !w.tokens.Live(carols) {
		t.Error("a refused removal removed the token")
	}
}

func TestUserListShowsEveryAccount(t *testing.T) {
	w := startManageWorkspace(t, "alice", "dave")
	alice, bob := newSigner(t), newSigner(t)
	w.enrol(t, "alice", alice)
	w.enrolKey(t, "bob", bob)
	ca := w.dial(t, "alice", alice)

	r := manageOn(t, ca, enrol.Request{Op: enrol.OpUserList})
	wantOK(t, r)
	byName := map[string]enrol.User{}
	for _, u := range r.Users {
		byName[u.Name] = u
	}
	a, b, d := byName["alice"], byName["bob"], byName["dave"]
	if !a.Admin || a.State != enrol.StateConnected || len(a.Sources) != 1 || !a.Sources[0].Operator || a.UID == 0 {
		t.Errorf("alice: %+v", a)
	}
	if b.Admin || b.State != enrol.StateEnrolled || len(b.Sources) != 1 || b.Sources[0].Operator || b.Sources[0].Keys != 1 {
		t.Errorf("bob: %+v", b)
	}
	if !d.Admin || d.State != enrol.StateNotEnrolled || d.UID != 0 {
		t.Errorf("dave, named but never enrolled: %+v", d)
	}

	wantRefused(t, manageOn(t, w.dial(t, "bob", bob), enrol.Request{Op: enrol.OpUserList}), enrol.CodeDenied, "list the accounts")
}

func TestUserRemoveRefusesRunningContainersUnlessForced(t *testing.T) {
	w := startManageWorkspace(t, "alice")
	alice, bob := newSigner(t), newSigner(t)
	w.enrol(t, "alice", alice)
	w.enrolKey(t, "bob", bob)
	ca, cb := w.dial(t, "alice", alice), w.dial(t, "bob", bob)
	before, _ := w.store.Lookup("bob")
	bobs := manageOn(t, cb, enrol.Request{Op: enrol.OpTokenCreate}).Token.ID

	w.targets.running["bob"] = 2
	wantRefused(t, manageOn(t, ca, enrol.Request{Op: enrol.OpUserRemove, Account: "bob"}), enrol.CodeForce,
		"bob's daemon is running 2 container(s)")
	w.targets.running["bob"] = -1
	wantRefused(t, manageOn(t, ca, enrol.Request{Op: enrol.OpUserRemove, Account: "bob"}), enrol.CodeForce,
		"cannot tell whether bob's daemon")
	assertOpen(t, cb, "a refused removal")
	if len(w.targets.reset) != 0 {
		t.Fatalf("a refused removal reset %v", w.targets.reset)
	}

	wantOK(t, manageOn(t, ca, enrol.Request{Op: enrol.OpUserRemove, Account: "bob", Force: true}))
	assertClosed(t, cb, "bob was removed")
	if got := strings.Join(w.targets.reset, ","); got != "bob purge=false" {
		t.Errorf("reset %q, want bob's daemon without its storage", got)
	}
	after, ok := w.store.Lookup("bob")
	if !ok || len(after.Keys) != 0 || after.UID != before.UID {
		t.Errorf("bob after removal: %+v, want revoked with uid %d", after, before.UID)
	}
	if w.tokens.Live(bobs) {
		t.Error("a token bound to a removed account still redeems")
	}
	if _, _, err := w.login("bob", bob); err == nil {
		t.Error("a removed account still authenticates")
	}
}

// A token of the account mid-redemption is withdrawn too, so the redemption
// cannot complete and bring the account back.
func TestUserRemoveWithdrawsAClaimedToken(t *testing.T) {
	w := startManageWorkspace(t, "alice")
	alice := newSigner(t)
	w.enrol(t, "alice", alice)
	w.enrolKey(t, "bob", newSigner(t))
	id, secret := w.mint(t, "bob")
	claim, err := w.tokens.Consume(id, secret)
	if err != nil {
		t.Fatal(err)
	}

	wantOK(t, manageOn(t, w.dial(t, "alice", alice), enrol.Request{Op: enrol.OpUserRemove, Account: "bob", Force: true}))
	if err := claim.Done(); err == nil {
		t.Error("a redemption of bob's token completed after bob was removed")
	}
}

func TestUserRemovePurgeTakesTheStorageAndThePortsAndKeepsTheUID(t *testing.T) {
	w := startManageWorkspace(t, "alice")
	ports := w.ports
	alice, bob := newSigner(t), newSigner(t)
	w.enrol(t, "alice", alice)
	w.enrolKey(t, "bob", bob)
	before, _ := w.store.Lookup("bob")
	if _, err := ports.For("bob", before.UID, "aabbccdd"); err != nil {
		t.Fatal(err)
	}
	ca := w.dial(t, "alice", alice)

	w.targets.running["bob"] = 1
	wantRefused(t, manageOn(t, ca, enrol.Request{Op: enrol.OpUserRemove, Account: "bob", Purge: true}), enrol.CodeForce,
		"bob's daemon is running 1 container(s)")
	wantOK(t, manageOn(t, ca, enrol.Request{Op: enrol.OpUserRemove, Account: "bob", Purge: true, Force: true}))

	if got := strings.Join(w.targets.reset, ","); got != "bob purge=true" {
		t.Errorf("reset %q, want bob's daemon with its storage", got)
	}
	if _, known, _ := ports.Lookup("bob", "aabbccdd"); known {
		t.Error("bob's machine is still in clientports")
	}
	after, ok := w.store.Lookup("bob")
	if !ok || len(after.Keys) != 0 || after.UID != before.UID {
		t.Errorf("bob after a purge: %+v, want revoked with uid %d", after, before.UID)
	}
	if known, err := w.store.Known("bob"); err != nil || !known {
		t.Errorf("the name bob is free again (%v); the uidmap must keep it", err)
	}
}

// Cleaning an expired run starts its account's daemon, and the run's volumes
// name its port until a purge removes that daemon's storage. So a removed
// account's runs keep their ports, uncleaned, unless a purge succeeded.
func TestUserRemoveKeepsTheAccountsRunsUntilAPurge(t *testing.T) {
	for _, tc := range []struct {
		name     string
		purge    bool
		resetErr error
		mode     string
		dropped  bool
	}{
		{"without a purge", false, nil, "", false},
		{"with a purge", true, nil, "", true},
		{"with a purge that failed", true, errors.New("docker: no such volume"), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := startManageWorkspace(t, "alice")
			w.targets.resetErr, w.targets.mode = tc.resetErr, tc.mode
			var mu sync.Mutex
			var cleaned []string
			w.runs.Grace = time.Nanosecond
			w.runs.Cleanup = func(_ context.Context, account, _ string) error {
				mu.Lock()
				defer mu.Unlock()
				cleaned = append(cleaned, account)
				return nil
			}
			alice, bob := newSigner(t), newSigner(t)
			w.enrol(t, "alice", alice)
			w.enrolKey(t, "bob", bob)
			release, err := w.runs.Attach("bob", "aabbccdd")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.runs.Port("bob", "aabbccdd"); err != nil {
				t.Fatal(err)
			}
			release()

			manageOn(t, w.dial(t, "alice", alice), enrol.Request{Op: enrol.OpUserRemove, Account: "bob", Force: true, Purge: tc.purge})
			w.runs.Sweep(t.Context())

			mu.Lock()
			defer mu.Unlock()
			if len(cleaned) != 0 {
				t.Errorf("cleaned a run of %v after removing bob, which starts bob's daemon again", cleaned)
			}
			if _, held, _ := w.ports.Lookup("bob", "aabbccdd"); held == tc.dropped {
				t.Errorf("bob's run holds its port: %t, want %t", held, !tc.dropped)
			}
		})
	}
}

// A purge forgets the account's ports only once the volumes naming them are
// gone, which a failed reset or a shared daemon leaves in place.
func TestUserRemovePurgeKeepsThePortsWhileTheVolumesStay(t *testing.T) {
	for _, tc := range []struct {
		name     string
		resetErr error
		mode     string
	}{
		{"a failed reset", errors.New("docker: no such volume"), ""},
		{"a shared daemon", nil, workspace.ModeShared},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := startManageWorkspace(t, "alice")
			w.targets.resetErr, w.targets.mode = tc.resetErr, tc.mode
			alice, bob := newSigner(t), newSigner(t)
			w.enrol(t, "alice", alice)
			w.enrolKey(t, "bob", bob)
			b, _ := w.store.Lookup("bob")
			if _, err := w.ports.For("bob", b.UID, "aabbccdd"); err != nil {
				t.Fatal(err)
			}
			manageOn(t, w.dial(t, "alice", alice), enrol.Request{Op: enrol.OpUserRemove, Account: "bob", Force: true, Purge: true})
			if _, known, _ := w.ports.Lookup("bob", "aabbccdd"); !known {
				t.Error("bob's machine left clientports while volumes may still name its port")
			}
		})
	}
}

func TestUserRemoveRefusals(t *testing.T) {
	w := startManageWorkspace(t, "alice")
	alice, dave := newSigner(t), newSigner(t)
	w.enrol(t, "alice", alice)
	w.enrol(t, "dave", dave)
	ca := w.dial(t, "alice", alice)

	wantRefused(t, manageOn(t, ca, enrol.Request{Op: enrol.OpUserRemove, Account: "alice", Force: true}), enrol.CodeDenied,
		"cannot remove their own account")
	wantRefused(t, manageOn(t, ca, enrol.Request{Op: enrol.OpUserRemove, Account: "dave", Force: true}), enrol.CodeDenied,
		"dave's keys are in "+w.keysDir)
	wantRefused(t, manageOn(t, ca, enrol.Request{Op: enrol.OpUserRemove, Account: "nobody"}), enrol.CodeUnknown, "no account nobody")
	wantRefused(t, manageOn(t, w.dial(t, "dave", dave), enrol.Request{Op: enrol.OpUserRemove, Account: "alice"}), enrol.CodeDenied,
		"only an admin can remove an account")
}

func TestRemovingAnAdminStillNamedSaysSo(t *testing.T) {
	w := startManageWorkspace(t, "alice", "carol")
	alice := newSigner(t)
	w.enrol(t, "alice", alice)
	w.enrolKey(t, "carol", newSigner(t))

	r := manageOn(t, w.dial(t, "alice", alice), enrol.Request{Op: enrol.OpUserRemove, Account: "carol"})
	wantOK(t, r)
	if len(r.Notices) != 1 || r.Notices[0].Fix != "remove carol from WORKSPACE_ADMINS" {
		t.Fatalf("notices %+v", r.Notices)
	}
}

func TestAnUnboundTokenRefusesAnAdminsName(t *testing.T) {
	w := startManageWorkspace(t, "carol")
	id, secret := w.mint(t, "")
	wantCode(t, w.redeem(t, id, newSigner(t), enrol.RedeemRequest{Secret: secret, Account: "Carol"}), enrol.CodeName)
	if reply := w.redeem(t, id, newSigner(t), enrol.RedeemRequest{Secret: secret, Account: "dave"}); reply.Error != nil {
		t.Fatalf("dave: %+v", reply.Error)
	}
}

func TestKeyOperations(t *testing.T) {
	w := startManageWorkspace(t)
	bob, spare := newSigner(t), newSigner(t)
	w.enrol(t, "bob", bob)
	cb := w.dial(t, "bob", bob)
	line := string(ssh.MarshalAuthorizedKey(spare.PublicKey()))
	bobFP, spareFP := ssh.FingerprintSHA256(bob.PublicKey()), ssh.FingerprintSHA256(spare.PublicKey())

	r := manageOn(t, cb, enrol.Request{Op: enrol.OpKeyList})
	if len(r.Keys) != 1 || !r.Keys[0].Current || !r.Keys[0].Operator || r.Keys[0].Fingerprint != bobFP {
		t.Fatalf("keys %+v", r.Keys)
	}

	wantOK(t, manageOn(t, cb, enrol.Request{Op: enrol.OpKeyAdd, Key: line}))
	if r := manageOn(t, cb, enrol.Request{Op: enrol.OpKeyAdd, Key: line}); len(r.Notices) != 1 {
		t.Errorf("a second add: %+v", r.Notices)
	}
	r = manageOn(t, cb, enrol.Request{Op: enrol.OpKeyList})
	if len(r.Keys) != 2 || r.Keys[1].Fingerprint != spareFP || r.Keys[1].Operator || r.Keys[1].Dir != w.enrolled {
		t.Fatalf("keys %+v", r.Keys)
	}

	wantRefused(t, manageOn(t, cb, enrol.Request{Op: enrol.OpKeyRemove, Fingerprint: bobFP, Force: true}), enrol.CodeDenied,
		"is in "+w.keysDir)
	wantOK(t, manageOn(t, cb, enrol.Request{Op: enrol.OpKeyRemove, Fingerprint: spareFP}))
	wantRefused(t, manageOn(t, cb, enrol.Request{Op: enrol.OpKeyRemove, Fingerprint: spareFP}), enrol.CodeUnknown, "has no key")
	wantRefused(t, manageOn(t, cb, enrol.Request{Op: enrol.OpKeyAdd, Key: "not a key"}), enrol.CodeFailed, "not a public key")
	wantRefused(t, manageOn(t, cb, enrol.Request{Op: enrol.OpKeyAdd, Account: "alice", Key: line}), enrol.CodeDenied,
		"add a key to another account")
}

// The reply arrives before the revocation closes the connection it went out on.
func TestRemovingTheKeyThisConnectionUsesRepliesFirst(t *testing.T) {
	w := startManageWorkspace(t)
	bob := newSigner(t)
	w.enrolKey(t, "bob", bob)
	cb := w.dial(t, "bob", bob)
	fp := ssh.FingerprintSHA256(bob.PublicKey())

	wantRefused(t, manageOn(t, cb, enrol.Request{Op: enrol.OpKeyRemove, Fingerprint: fp}), enrol.CodeForce, "connected with")
	wantOK(t, manageOn(t, cb, enrol.Request{Op: enrol.OpKeyRemove, Fingerprint: fp, Force: true}))
	assertClosed(t, cb, "its key was removed")
}

func TestTheLastAdminKeepsTheirLastKey(t *testing.T) {
	w := startManageWorkspace(t, "alice")
	alice := newSigner(t)
	w.enrolKey(t, "alice", alice)
	ca := w.dial(t, "alice", alice)

	wantRefused(t, manageOn(t, ca, enrol.Request{Op: enrol.OpKeyRemove, Fingerprint: ssh.FingerprintSHA256(alice.PublicKey()), Force: true}),
		enrol.CodeDenied, "alice is the last admin with a key")
	assertOpen(t, ca, "a refused removal")
}

func TestAnUnknownOperationIsNamed(t *testing.T) {
	w := startManageWorkspace(t)
	bob := newSigner(t)
	w.enrol(t, "bob", bob)
	wantRefused(t, manageOn(t, w.dial(t, "bob", bob), enrol.Request{Op: "user.purge"}), enrol.CodeUnknown, `"user.purge"`)
}
