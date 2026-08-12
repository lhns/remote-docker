package sshd

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/lhns/remote-docker/space/accounts"
	"github.com/lhns/remote-docker/agent/internal/daemons"
	"github.com/lhns/remote-docker/pkg/workspace"
)

// Routing an account to its daemon is the one thing here that fails by
// SUCCEEDING: send a session to the wrong daemon and every command works,
// against somebody else's containers. Nothing errors, nothing is logged, and
// the integration suite's cross-account checks are the only thing between that
// and a user seeing work that is not theirs.
//
// It could not be tested at all until the resolution moved behind daemons
// .Targets: the call sites took a concrete *daemons.Manager, which needs a real
// docker daemon to do anything. That is why this file exists.

// fakeTargets is a resolver that records what it was asked for.
type fakeTargets struct {
	mu        sync.Mutex
	byAccount map[string]daemons.Target
	asked     []string
	warmed    []string

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

// Every path that reaches a daemon asks for the account that owns the session,
// and never a fixed one. A resolver that ignored its argument, which is what
// a mode branch left behind would amount to, fails here.
func TestEveryPathResolvesTheSessionsOwnAccount(t *testing.T) {
	for _, account := range []string{"alice", "bob"} {
		targets := twoAccounts()
		want := targets.byAccount[account]

		got, err := targets.Ensure(context.Background(), account)
		if err != nil {
			t.Fatalf("Ensure(%s): %v", account, err)
		}
		if got.Socket != want.Socket || got.NetNSPath != want.NetNSPath || got.Root != want.Root {
			t.Fatalf("%s resolved to %+v, want %+v", account, got, want)
		}
		if len(targets.asked) != 1 || targets.asked[0] != account {
			t.Fatalf("the resolver was asked for %v, want [%s]", targets.asked, account)
		}
	}
}

// The four coordinates of a target belong to ONE daemon and must never be
// mixed. Reading alice's root against bob's host would relocate a mountpoint
// into the wrong container's filesystem: a root process writing to a path
// that exists and belongs to somebody else.
func TestATargetIsNotMixedBetweenAccounts(t *testing.T) {
	targets := twoAccounts()

	alice, _ := targets.Ensure(context.Background(), "alice")
	bob, _ := targets.Ensure(context.Background(), "bob")

	if alice.Socket == bob.Socket || alice.Root == bob.Root || alice.NetNSPath == bob.NetNSPath {
		t.Fatal("two accounts resolved to the same daemon")
	}
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
	if shared.Mode() != daemons.ModeShared {
		t.Errorf("Mode = %q, want %q", shared.Mode(), daemons.ModeShared)
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

// The info reply must not wait for a daemon. It is the client's first round
// trip and two of its fields come from running the docker CLI; making them
// Ensure would turn every new connection into a wait for a cold dind, for a
// version string the client only displays.
//
// So: Lookup, and a miss is an answer. This is the property a careless
// unification destroys silently: everything still works, just slowly, and
// only on the first connection after a restart.
func TestInfoQueriesNeverStartADaemon(t *testing.T) {
	targets := twoAccounts()
	targets.missing = true

	if _, ok := targets.Lookup(context.Background(), "alice"); ok {
		t.Fatal("Lookup answered for a daemon that is not up")
	}

	// Ensure is what would start one, and nothing in the info path may call it.
	targets.err = errors.New("this daemon must not be started here")
	if _, err := targets.Ensure(context.Background(), "alice"); err == nil {
		t.Fatal("the fake did not fail as configured")
	}
}

// Authentication warms the account's own daemon, so the boot hides behind the
// round trips that follow rather than behind the user's first docker command.
func TestAuthenticationWarmsTheAccountsDaemon(t *testing.T) {
	targets := twoAccounts()
	targets.Warm("alice")

	if len(targets.warmed) != 1 || targets.warmed[0] != "alice" {
		t.Fatalf("warmed %v, want [alice]", targets.warmed)
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
	if s.cfg.Daemons.Mode() != daemons.ModeShared {
		t.Errorf("mode = %q, want %q", s.cfg.Daemons.Mode(), daemons.ModeShared)
	}
}
