package sshd

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/lhns/remote-docker/agent/internal/daemons"
	"github.com/lhns/remote-docker/core-agent/accounts"
	"github.com/lhns/remote-docker/core-agent/tokens"
	"github.com/lhns/remote-docker/core/enrol"
	"github.com/lhns/remote-docker/core/workspace"
)

// Redeeming a token, over real SSH connections.

type tokenWorkspace struct {
	*revokeWorkspace
	tokens  *tokens.Store
	limiter *tokens.Limiter
}

// startTokenWorkspace serves a workspace with a token store; opts change its
// Config before it starts.
func startTokenWorkspace(t *testing.T, enrolledDir string, opts ...func(*Config)) *tokenWorkspace {
	t.Helper()
	root := t.TempDir()
	w := &tokenWorkspace{
		revokeWorkspace: &revokeWorkspace{keysDir: filepath.Join(root, "keys"), enrolled: enrolledDir},
		tokens:          &tokens.Store{Dir: filepath.Join(root, "tokens")},
		limiter:         &tokens.Limiter{Burst: 3, Every: time.Hour},
	}
	if w.enrolled == "" {
		w.enrolled = filepath.Join(root, "enrolled")
		if err := os.MkdirAll(w.enrolled, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(w.keysDir, 0o755); err != nil {
		t.Fatal(err)
	}
	w.store = accounts.New([]string{w.keysDir}, w.enrolled, t.TempDir(), workspace.DefaultMapping(), fakeProvisioner{}, nil)

	cfg := Config{
		Accounts: w.store,
		Mapping:  workspace.DefaultMapping(),
		Daemons:  daemons.Shared(""),
		HostKeys: []ssh.Signer{newSigner(t)},
		Tokens:   w.tokens,
		Limiter:  w.limiter,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.ServeListener(l) }()
	t.Cleanup(func() { _ = s.Close(); _ = l.Close() })
	w.addr = l.Addr().String()
	return w
}

func (w *tokenWorkspace) mint(t *testing.T, account string) (id, secret string) {
	t.Helper()
	token, _, err := w.tokens.Mint(account, "operator", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	id, secret, err = enrol.ParseToken(token)
	if err != nil {
		t.Fatal(err)
	}
	return id, secret
}

// login dials as user, reporting whether a banner arrived.
func (w *tokenWorkspace) login(user string, key ssh.Signer) (*ssh.Client, bool, error) {
	var banner atomic.Bool
	c, err := ssh.Dial("tcp", w.addr, &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(key)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		BannerCallback: func(msg string) error {
			banner.Store(enrol.IsBanner(msg))
			return nil
		},
	})
	return c, banner.Load(), err
}

func (w *tokenWorkspace) redeemer(t *testing.T, id string, key ssh.Signer) *ssh.Client {
	t.Helper()
	c, _, err := w.login(enrol.LoginPrefix+id, key)
	if err != nil {
		t.Fatalf("a token login was refused: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func redeemOn(t *testing.T, c *ssh.Client, req enrol.RedeemRequest) enrol.RedeemReply {
	t.Helper()
	s, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	body, _ := json.Marshal(req)
	s.Stdin = bytes.NewReader(body)
	out, err := s.Output(enrol.RedeemCommand)
	if err != nil {
		t.Fatalf("%s: %v", enrol.RedeemCommand, err)
	}
	var reply enrol.RedeemReply
	if err := json.Unmarshal(out, &reply); err != nil {
		t.Fatalf("reply %q: %v", out, err)
	}
	return reply
}

func (w *tokenWorkspace) redeem(t *testing.T, id string, key ssh.Signer, req enrol.RedeemRequest) enrol.RedeemReply {
	t.Helper()
	return redeemOn(t, w.redeemer(t, id, key), req)
}

func wantCode(t *testing.T, reply enrol.RedeemReply, code string) {
	t.Helper()
	if reply.Error == nil || reply.Error.Code != code {
		t.Fatalf("reply %+v (error %+v), want code %q", reply, reply.Error, code)
	}
}

func TestABoundTokenEnrolsTheKeyThatLoggedIn(t *testing.T) {
	w := startTokenWorkspace(t, "")
	id, secret := w.mint(t, "alice")
	key := newSigner(t)

	reply := w.redeem(t, id, key, enrol.RedeemRequest{Secret: secret, Comment: "alice@laptop"})
	if reply.Error != nil || reply.Account != "alice" || !reply.Created {
		t.Fatalf("reply %+v, error %+v", reply, reply.Error)
	}
	data, err := os.ReadFile(filepath.Join(w.enrolled, "alice.pub"))
	if err != nil || !strings.Contains(string(data), "alice@laptop") {
		t.Fatalf("the enrolled file is %q: %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(w.keysDir, "alice.pub")); !os.IsNotExist(err) {
		t.Error("the redeem wrote into the operator's directory")
	}
	w.dial(t, "alice", key)

	// Used: the login itself is refused now, with the banner, so the client
	// says the token was refused rather than that the workspace is old.
	_, banner, err := w.login(enrol.LoginPrefix+id, key)
	if err == nil || !banner {
		t.Errorf("a used token: err %v, banner %v; want a refusal with the banner", err, banner)
	}
}

// A token withdrawn while its redemption is writing the key (user rm) enrols
// nothing: the redemption cannot complete its claim, and takes the key out.
func TestATokenWithdrawnMidRedemptionEnrolsNothing(t *testing.T) {
	w := startTokenWorkspace(t, "")
	if err := w.store.CreateAccount("alice", newSigner(t).PublicKey(), "alice@desk"); err != nil {
		t.Fatal(err)
	}
	id, secret := w.mint(t, "alice")

	var armed atomic.Bool
	armed.Store(true)
	w.store.Subscribe(func() { // runs inside the redemption's key write
		if armed.CompareAndSwap(true, false) {
			if err := w.tokens.Revoke(id); err != nil {
				t.Errorf("withdrawing the claimed token: %v", err)
			}
		}
	})
	key := newSigner(t)
	c := w.redeemer(t, id, key)
	wantCode(t, redeemOn(t, c, enrol.RedeemRequest{Secret: secret}), enrol.CodeRefused)
	if a, _ := w.store.Lookup("alice"); a.Authorized(key.PublicKey()) {
		t.Error("a withdrawn token's key is enrolled")
	}
}

func TestABoundTokenAddsAKeyToAnAccountThatExists(t *testing.T) {
	w := startTokenWorkspace(t, "")
	first, second := newSigner(t), newSigner(t)
	w.enrol(t, "alice", first)
	id, secret := w.mint(t, "alice")

	reply := w.redeem(t, id, second, enrol.RedeemRequest{Secret: secret, Account: "Alice"})
	if reply.Error != nil || reply.Account != "alice" || reply.Created {
		t.Fatalf("reply %+v, error %+v", reply, reply.Error)
	}
	w.dial(t, "alice", first)
	w.dial(t, "alice", second)
}

func TestABoundTokenRefusesAnotherName(t *testing.T) {
	w := startTokenWorkspace(t, "")
	id, secret := w.mint(t, "alice")
	wantCode(t, w.redeem(t, id, newSigner(t), enrol.RedeemRequest{Secret: secret, Account: "bob"}), enrol.CodeName)
	if !w.tokens.Live(id) {
		t.Error("a refused name spent the token")
	}
}

func TestAnUnboundTokenOnlyCreates(t *testing.T) {
	w := startTokenWorkspace(t, "")
	w.enrol(t, "alice", newSigner(t))
	id, secret := w.mint(t, "")

	for _, name := range []string{"", "alice", "Alice", "root"} {
		wantCode(t, w.redeem(t, id, newSigner(t), enrol.RedeemRequest{Secret: secret, Account: name}), enrol.CodeName)
	}
	if !w.tokens.Live(id) {
		t.Fatal("a refused name spent the token")
	}

	key := newSigner(t)
	reply := w.redeem(t, id, key, enrol.RedeemRequest{Secret: secret, Account: "bob"})
	if reply.Error != nil || reply.Account != "bob" || !reply.Created {
		t.Fatalf("reply %+v, error %+v", reply, reply.Error)
	}
	w.dial(t, "bob", key)
}

// A name the uidmap remembers was an account once, and an unbound token
// never hands it to somebody new.
func TestAnUnboundTokenNeverReusesAName(t *testing.T) {
	w := startTokenWorkspace(t, "")
	w.enrol(t, "carol", newSigner(t))
	if err := os.Remove(filepath.Join(w.keysDir, "carol.pub")); err != nil {
		t.Fatal(err)
	}
	w.sync(t)

	id, secret := w.mint(t, "")
	wantCode(t, w.redeem(t, id, newSigner(t), enrol.RedeemRequest{Secret: secret, Account: "carol"}), enrol.CodeName)
}

func TestAWrongSecretIsRefusedAndCounted(t *testing.T) {
	w := startTokenWorkspace(t, "")
	id, secret := w.mint(t, "alice")
	wrong := strings.Repeat("A", len(secret))

	for range w.limiter.Burst {
		wantCode(t, w.redeem(t, id, newSigner(t), enrol.RedeemRequest{Secret: wrong}), enrol.CodeRefused)
	}
	// Now even the right secret waits, so guessing gains nothing by speed.
	wantCode(t, w.redeem(t, id, newSigner(t), enrol.RedeemRequest{Secret: secret}), enrol.CodeBusy)
	if !w.tokens.Live(id) {
		t.Error("refusals spent the token")
	}
}

func TestAWorkspaceThatCannotStoreKeysSpendsNothing(t *testing.T) {
	w := startTokenWorkspace(t, filepath.Join(t.TempDir(), "missing"))
	id, secret := w.mint(t, "alice")
	reply := w.redeem(t, id, newSigner(t), enrol.RedeemRequest{Secret: secret})
	wantCode(t, reply, enrol.CodeStorage)
	if !strings.Contains(reply.Error.Fix, "WORKSPACE_ENROLLED_KEYS_DIR") {
		t.Errorf("the fix does not name the setting: %q", reply.Error.Fix)
	}
	if !w.tokens.Live(id) {
		t.Error("a workspace that could not store the key spent the token")
	}
}

func TestBannerOnlyForATokenLogin(t *testing.T) {
	w := startTokenWorkspace(t, "")
	key := newSigner(t)
	w.enrol(t, "alice", key)

	c, banner, err := w.login("alice", key)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	if banner {
		t.Error("an account login was sent the enrolment banner")
	}
	if _, banner, err := w.login(enrol.LoginPrefix+"aaaaaaaa", key); err == nil || !banner {
		t.Errorf("an unknown token: err %v, banner %v; want a refusal with the banner", err, banner)
	}
	// The prefix is checked before folding, so it cannot reach an account.
	if _, _, err := w.login("token-aaaaaaaa", key); err == nil {
		t.Error("a folded token login authenticated")
	}
}

// A token login has no account, so everything an account can do is refused,
// and the redeem runs once.
func TestATokenLoginCanOnlyRedeem(t *testing.T) {
	w := startTokenWorkspace(t, "")
	owner := newSigner(t)
	w.enrol(t, "alice", owner)
	id, secret := w.mint(t, "bob")
	c := w.redeemer(t, id, owner) // alice's own key, which buys nothing here

	for _, cmd := range []string{workspace.InfoCommand, workspace.DialStdioCommand, "id", "", enrol.EnrolCommand} {
		s, err := c.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		if out, err := s.CombinedOutput(cmd); err == nil {
			t.Errorf("%q ran on a token login: %s", cmd, out)
		}
		_ = s.Close()
	}
	if ok, _, _ := c.SendRequest(workspace.RunRequest, true, []byte("run")); ok {
		t.Error("a token login named a run")
	}
	if ok, _, _ := c.SendRequest("tcpip-forward", true, ssh.Marshal(struct {
		Addr string
		Port uint32
	}{"127.0.0.1", 0})); ok {
		t.Error("a token login bound a reverse forward")
	}
	if ch, _, err := c.OpenChannel("direct-tcpip", ssh.Marshal(struct {
		Host     string
		Port     uint32
		OrigHost string
		OrigPort uint32
	}{"127.0.0.1", 22, "127.0.0.1", 1})); err == nil {
		_ = ch.Close()
		t.Error("a token login opened a direct-tcpip channel")
	}

	wantCode(t, redeemOn(t, c, enrol.RedeemRequest{Secret: strings.Repeat("A", len(secret))}), enrol.CodeRefused)
	s, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	s.Stdin = strings.NewReader(`{"secret":"` + secret + `"}`)
	if out, err := s.Output(enrol.RedeemCommand); err == nil {
		t.Errorf("a second redeem ran on one connection: %s", out)
	}
	if _, ok := w.store.Lookup("bob"); ok {
		t.Error("bob was enrolled")
	}
}
