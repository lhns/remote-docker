package sshd

import (
	"context"
	"net"
	"testing"
)

// With no Manager the behaviour must be exactly what it was: bind here, dial
// here. The shared daemon lives in this namespace, so anything else would
// break the mode kept as the escape hatch.
func TestForwardsUseThisNamespaceWithoutAManager(t *testing.T) {
	s := &Server{cfg: Config{}}
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
