package netns

import (
	"errors"
	"net"
	"testing"
)

// The empty path means "this namespace" and must not touch a system call.
//
// This is the shared-daemon mode (ADR 0012) travelling through the same code
// as the per-account one, and it is the reason the call sites in sshd have no
// mode branch left. If this ever started calling enter, the agent would refuse
// every forward on a shared-daemon workspace, and on the development machine,
// where enter is unsupported, these tests are the only thing that would say so.
func TestDoWithNoPathRunsHere(t *testing.T) {
	ran := false
	if err := Do("", func() error {
		ran = true
		return nil
	}); err != nil {
		t.Fatalf("Do with no path: %v", err)
	}
	if !ran {
		t.Fatal("Do with no path did not run the function")
	}
}

// The function's own error is returned, not swallowed or wrapped into
// something about namespaces -- a bind failure must read as a bind failure.
func TestDoWithNoPathReturnsTheFunctionsError(t *testing.T) {
	want := errors.New("the address was already in use")
	if got := Do("", func() error { return want }); !errors.Is(got, want) {
		t.Fatalf("Do returned %v, want %v", got, want)
	}
}

func TestListenAndDialWithNoPath(t *testing.T) {
	l, err := Listen("", "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen with no path: %v", err)
	}
	defer func() { _ = l.Close() }()

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			close(accepted)
			return
		}
		accepted <- c
	}()

	c, err := Dial("", "tcp", l.Addr().String())
	if err != nil {
		t.Fatalf("Dial with no path: %v", err)
	}
	defer func() { _ = c.Close() }()

	server, ok := <-accepted
	if !ok {
		t.Fatal("the listener never accepted the connection")
	}
	_ = server.Close()
}
