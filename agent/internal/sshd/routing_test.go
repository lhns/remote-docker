package sshd

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	gssh "github.com/gliderlabs/ssh"
	"golang.org/x/crypto/ssh"

	"github.com/lhns/remote-docker/agent/internal/daemons"
	"github.com/lhns/remote-docker/agent/internal/unions"
	"github.com/lhns/remote-docker/core-agent/accounts"
	"github.com/lhns/remote-docker/core/workspace"
)

// Routing an account to its daemon is the one thing here that fails by
// SUCCEEDING: send a session to the wrong daemon and every command works,
// against somebody else's containers. Nothing errors, nothing is logged, and
// the integration suite's cross-account checks are the only thing between that
// and a user seeing work that is not theirs.
//
// Every test here goes through Server with a resolver that records what it was
// asked, because that is the only thing that can fail: a resolver asked
// directly answers what it was built to answer. The forwards are in
// forward_tcpip_test.go; the info reply and authentication are here.

// fakeTargets is a resolver that records what it was asked for.
type fakeTargets struct {
	mu        sync.Mutex
	byAccount map[string]daemons.Target

	// asked is every account resolved, by Ensure or Lookup; ensured is the
	// ones Ensure was asked for, which is what would start a daemon.
	asked   []string
	ensured []string
	warmed  []string

	// missing makes Lookup answer false, which is what a daemon that has not
	// booted yet looks like.
	missing bool

	// err makes Ensure fail, which is a daemon that will not start.
	err error
}

func (f *fakeTargets) Ensure(_ context.Context, account string) (daemons.Target, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, account)
	f.ensured = append(f.ensured, account)
	if f.err != nil {
		return daemons.Target{}, f.err
	}
	return f.byAccount[account], nil
}

func (f *fakeTargets) Lookup(_ context.Context, account string) (daemons.Target, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, account)
	if f.missing {
		return daemons.Target{}, false
	}
	t, ok := f.byAccount[account]
	return t, ok
}

func (f *fakeTargets) Warm(account string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.warmed = append(f.warmed, account)
}

func (f *fakeTargets) Mode() string { return "fake" }

func twoAccounts() *fakeTargets {
	return &fakeTargets{byAccount: map[string]daemons.Target{
		"alice": {
			Socket:    "/run/rd/alice/docker.sock",
			Host:      "unix:///run/rd/alice/docker.sock",
			NetNSPath: "/proc/11/ns/net",
			Root:      "/proc/11/root",
		},
		"bob": {
			Socket:    "/run/rd/bob/docker.sock",
			Host:      "unix:///run/rd/bob/docker.sock",
			NetNSPath: "/proc/22/ns/net",
			Root:      "/proc/22/root",
		},
	}}
}

// The shared daemon answers for everybody, which is what that mode means, and
// says so with empty redirections rather than with equivalent values: an empty
// Host is "the default socket is already right", and an empty NetNSPath is
// "this namespace" (netns.Do).
func TestSharedAnswersForEveryAccount(t *testing.T) {
	shared := daemons.Shared("")

	alice, err := shared.Ensure(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	bob, _ := shared.Ensure(context.Background(), "bob")

	if alice != bob {
		t.Fatalf("the shared daemon answered differently for two accounts: %+v vs %+v", alice, bob)
	}
	if alice.Socket != "/var/run/docker.sock" {
		t.Errorf("socket = %q, want the default", alice.Socket)
	}
	if alice.Host != "" {
		t.Errorf("Host = %q, want empty: a login shell should get no DOCKER_HOST here", alice.Host)
	}
	if alice.NetNSPath != "" {
		t.Errorf("NetNSPath = %q, want empty: the shared daemon is in our own namespace", alice.NetNSPath)
	}
	if alice.Root != "/" {
		t.Errorf("Root = %q, want /: its mountpoints are already ours", alice.Root)
	}
	if shared.Mode() != workspace.ModeShared {
		t.Errorf("Mode = %q, want %q", shared.Mode(), workspace.ModeShared)
	}
}

// A socket the caller chose is honoured. The deployment sets this, and
// defaulting over it would send every session to a daemon nobody asked for.
func TestSharedHonoursTheSocketItWasGiven(t *testing.T) {
	target, _ := daemons.Shared("/tmp/other.sock").Ensure(context.Background(), "alice")
	if target.Socket != "/tmp/other.sock" {
		t.Errorf("socket = %q, want the one configured", target.Socket)
	}
}

// The info reply must not wait for a daemon, and must ask for the session's
// own account. It is the client's first round trip and its fields come from
// running the docker CLI; making them Ensure would turn every new connection
// into a wait for a cold dind, for a version string the client only displays.
//
// So: Lookup, and a miss is an answer. The daemon here has not booted, which
// also keeps the docker CLI from being run.
func TestInfoQueriesNeverStartADaemon(t *testing.T) {
	for _, account := range []string{"alice", "bob"} {
		targets := twoAccounts()
		targets.missing = true
		targets.err = errors.New("this daemon must not be started here")
		s := &Server{cfg: Config{Daemons: targets, Unions: &unions.Manager{}}}

		ctx := context.Background()
		if got := s.dockerVersion(ctx, account); got != workspace.DockerUnavailable {
			t.Errorf("dockerVersion = %q for a daemon that is not up, want %q", got, workspace.DockerUnavailable)
		}
		if got := s.storageDriver(ctx, account); got != "" {
			t.Errorf("storageDriver = %q for a daemon that is not up, want empty", got)
		}
		if got := s.unionCapability(ctx, account); got != "" {
			t.Errorf("unionCapability = %q for a daemon that is not up, want empty", got)
		}

		if len(targets.ensured) != 0 {
			t.Errorf("an info query started a daemon: Ensure was asked for %v", targets.ensured)
		}
		for _, asked := range targets.asked {
			if asked != account {
				t.Errorf("the resolver was asked for %q during %s's info reply", asked, account)
			}
		}
		if len(targets.asked) != 3 {
			t.Errorf("the resolver was asked %d times, want once per field: %v", len(targets.asked), targets.asked)
		}
	}
}

// Authentication warms the account's own daemon, so the boot hides behind the
// round trips that follow rather than behind the user's first docker command.
// A key that is refused warms nothing, or anybody could boot anybody's daemon.
func TestAuthenticationWarmsTheAccountsDaemon(t *testing.T) {
	keysDir := t.TempDir()
	key, other := generateKey(t), generateKey(t)
	if err := os.WriteFile(filepath.Join(keysDir, "alice.pub"), ssh.MarshalAuthorizedKey(key), 0o600); err != nil {
		t.Fatal(err)
	}
	store := accounts.New(keysDir, t.TempDir(), workspace.DefaultMapping(), fakeProvisioner{}, nil)
	if err := store.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	targets := twoAccounts()
	s := &Server{cfg: Config{Accounts: store, Daemons: targets}}

	if s.authenticate(newFakeContext("alice"), other) {
		t.Fatal("a key that is not enrolled was accepted")
	}
	if len(targets.warmed) != 0 {
		t.Fatalf("a refused key warmed %v", targets.warmed)
	}

	ctx := newFakeContext("alice")
	if !s.authenticate(ctx, key) {
		t.Fatal("alice's own key was refused")
	}
	if len(targets.warmed) != 1 || targets.warmed[0] != "alice" {
		t.Fatalf("warmed %v, want [alice]", targets.warmed)
	}

	// And the session now knows who it is, including which MACHINE, which is
	// derived from the key and not asserted by the client.
	account, ok := accountFor(ctx)
	if !ok || account.Name() != "alice" {
		t.Fatalf("the connection carries %+v, want alice", account)
	}
	if account.Client() != workspace.ClientID(key.Marshal()) {
		t.Errorf("client = %q, want the digest of the key that authenticated", account.Client())
	}
}

// A server built without a resolver serves the shared daemon rather than
// panicking on the first session. New is the only constructor, and a nil
// dereference at the first connection is a poor way to learn that.
func TestNewDefaultsToTheSharedDaemon(t *testing.T) {
	store := accounts.New(t.TempDir(), t.TempDir(),
		workspace.Mapping{UIDBase: workspace.DefaultUIDBase, PortBase: workspace.DefaultPortBase},
		nil, nil)

	s, err := New(Config{Accounts: store})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s.cfg.Daemons == nil {
		t.Fatal("Daemons is nil; a session would panic")
	}
	if s.cfg.Daemons.Mode() != workspace.ModeShared {
		t.Errorf("mode = %q, want %q", s.cfg.Daemons.Mode(), workspace.ModeShared)
	}
}

func generateKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// fakeProvisioner creates no unix account, which a test cannot do anyway.
type fakeProvisioner struct{}

func (fakeProvisioner) Ensure(name string, _ int, _ string) (string, string, error) {
	return name, "/home/" + name, nil
}

// fakeContext is a gssh.Context with no connection behind it: the user it
// claims, and a place for authenticate to record the account.
type fakeContext struct {
	context.Context
	sync.Mutex
	user   string
	values map[any]any
}

func newFakeContext(user string) *fakeContext {
	return &fakeContext{Context: context.Background(), user: user, values: map[any]any{}}
}

func (c *fakeContext) User() string                   { return c.user }
func (c *fakeContext) SessionID() string              { return "" }
func (c *fakeContext) ClientVersion() string          { return "" }
func (c *fakeContext) ServerVersion() string          { return "" }
func (c *fakeContext) RemoteAddr() net.Addr           { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }
func (c *fakeContext) LocalAddr() net.Addr            { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }
func (c *fakeContext) Permissions() *gssh.Permissions { return nil }
func (c *fakeContext) SetValue(key, value any)        { c.values[key] = value }

func (c *fakeContext) Value(key any) any {
	if v, ok := c.values[key]; ok {
		return v
	}
	return c.Context.Value(key)
}
