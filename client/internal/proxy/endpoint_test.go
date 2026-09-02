package proxy

import (
	"net"
	"sync"
	"testing"
)

// serve accepts and drops connections until the listener closes.
//
// Needed because a named pipe has no backlog: unlike a unix socket, nothing can
// connect to it until an Accept is outstanding, so a listener nobody is serving
// looks exactly like one that is not there. A real endpoint always has the
// proxy behind it, so serving here is the faithful shape as well as the
// portable one.
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
// That is the whole of the reported bug, and it is worth an assertion because
// of who it happens to. `remote-docker`'s own commands start a session when
// they find none, so from them the endpoint looks self-healing. A foreign
// client -- the stock Docker CLI, compose, buildx, Testcontainers, an IDE
// plugin -- calls connect() and gets ENOENT, knows nothing about sessions, and
// has no way to bring one back. The README presents exactly those tools as the
// endpoint's consumers.
//
// This pins TODAY's behaviour so the fix has something to flip. When the
// endpoint outlives its session, the second half of this test is what changes.
func TestEndpointDiesWithItsListener(t *testing.T) {
	endpoint := testEndpoint(t)

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
	endpoint := testEndpoint(t)

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
