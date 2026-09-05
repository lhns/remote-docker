package proxy

import (
	"net"
	"sync"
	"testing"

	"github.com/lhns/remote-docker/client/internal/endpointtest"
)

// serve accepts and drops connections until the listener closes.
//
// A named pipe has no backlog: nothing connects until an Accept is outstanding,
// so an unserved listener looks exactly like an absent one. A real endpoint
// always has the proxy behind it anyway.
func serve(t *testing.T, l net.Listener) {
	t.Helper()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	t.Cleanup(wg.Wait)
}

// The endpoint is bound by the session process and goes when it goes.
//
// remote-docker's own commands start a session when they find none, so from
// them it looks self-healing; a foreign client gets ENOENT and has no way back.
// That asymmetry is the bug. Pinned so a fix has something to flip.
func TestEndpointDiesWithItsListener(t *testing.T) {
	endpoint := endpointtest.Endpoint(t)

	l, err := Listen(endpoint)
	if err != nil {
		t.Fatalf("binding the endpoint: %v", err)
	}
	serve(t, l)
	if !Reachable(endpoint) {
		t.Fatal("a freshly bound endpoint is not reachable")
	}

	if err := l.Close(); err != nil {
		t.Fatalf("closing the endpoint: %v", err)
	}

	// Nothing holds the path once the listener is gone. A foreign client sees
	// this as "no such file or directory", which names neither the session nor
	// any way to restart it.
	if Reachable(endpoint) {
		t.Error("the endpoint outlived its listener, which this test exists to detect changing")
	}
}

// Binding again is what a `remote-docker` command does when it finds no
// session, and it is the asymmetry: the same endpoint that is dead for
// everybody else comes back for us.
func TestEndpointComesBackWhenSomethingBindsItAgain(t *testing.T) {
	endpoint := endpointtest.Endpoint(t)

	first, err := Listen(endpoint)
	if err != nil {
		t.Fatalf("binding the endpoint: %v", err)
	}
	serve(t, first)
	if err := first.Close(); err != nil {
		t.Fatalf("closing the endpoint: %v", err)
	}

	second, err := Listen(endpoint)
	if err != nil {
		t.Fatalf("rebinding the endpoint: %v", err)
	}
	defer func() { _ = second.Close() }()
	serve(t, second)

	if !Reachable(endpoint) {
		t.Error("the endpoint is not reachable after being bound again")
	}
}
