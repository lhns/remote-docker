package sshd

import (
	"context"
	"net"
	"testing"

	"github.com/lhns/remote-docker/internal/server/daemons"
)

// With the shared daemon the behaviour must be exactly what it was: bind here,
// dial here. That daemon lives in the agent's own namespace, so anything else
// would break the mode ADR 0012 keeps as the escape hatch.
//
// The mode arrives as a VALUE now rather than as a nil manager, and the
// namespace it names is the empty path -- see netns.Do. This test is what says
// the two spellings mean the same thing.
func TestForwardsUseThisNamespaceForTheSharedDaemon(t *testing.T) {
	s := &Server{cfg: Config{Daemons: daemons.Shared("")}}
	account := sessionAccount{name: "alice", uid: 10001}

	ln, err := s.listenFor(context.Background(), account, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listenFor: %v", err)
	}
	defer func() { _ = ln.Close() }()

	// Reachable from this namespace, which is the whole claim.
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("the listener is not reachable from the agent's namespace: %v", err)
	}
	_ = conn.Close()

	// And a local forward dials from here too.
	out, err := s.dialFor(context.Background(), account, ln.Addr().String())
	if err != nil {
		t.Fatalf("dialFor: %v", err)
	}
	_ = out.Close()
}

// A forward is bound in the namespace of the account that asked for it, and in
// no other. This is the assertion the per-account mode rests on: the reverse
// tunnel carries an unauthenticated NFS export, and binding it in the wrong
// namespace exposes one user's files to another with nothing failing.
func TestForwardsUseTheAskingAccountsNamespace(t *testing.T) {
	targets := &fakeTargets{
		byAccount: map[string]daemons.Target{
			"alice": {NetNSPath: "/proc/11/ns/net", Socket: "/run/rd/alice/docker.sock"},
			"bob":   {NetNSPath: "/proc/22/ns/net", Socket: "/run/rd/bob/docker.sock"},
		},
	}
	s := &Server{cfg: Config{Daemons: targets}}

	// The bind itself cannot succeed here -- entering a named namespace is
	// Linux-only and those pids do not exist -- and it does not need to. What
	// is under test is WHICH namespace was asked for.
	_, _ = s.listenFor(context.Background(), sessionAccount{name: "alice"}, "127.0.0.1:0")
	_, _ = s.dialFor(context.Background(), sessionAccount{name: "bob"}, "127.0.0.1:1")

	if got := targets.asked; len(got) != 2 || got[0] != "alice" || got[1] != "bob" {
		t.Fatalf("the resolver was asked for %v, want [alice bob]", got)
	}
}
