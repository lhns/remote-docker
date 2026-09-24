package tunnelserver

import (
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	gssh "github.com/gliderlabs/ssh"
	gossh "golang.org/x/crypto/ssh"
)

// reserving allows every forward and records what is given back.
type reserving struct {
	mu       sync.Mutex
	released []uint64
}

func (r *reserving) Allow(gssh.Context, string, uint32) (uint64, bool) { return 7, true }
func (r *reserving) Release(token uint64, _ string, _ uint32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.released = append(r.released, token)
}
func (r *reserving) Listen(_ gssh.Context, addr string) (net.Listener, error) {
	return net.Listen("tcp", addr)
}

// serveForwards starts an SSH server answering forwards with f, and dials it.
func serveForwards(t *testing.T, f *Forwards) func() *gossh.Client {
	t.Helper()
	srv := &gssh.Server{
		Handler: func(gssh.Session) {},
		RequestHandlers: map[string]gssh.RequestHandler{
			"tcpip-forward":        f.HandleRequest,
			"cancel-tcpip-forward": f.HandleRequest,
		},
	}
	ln := listen(t)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	return func() *gossh.Client {
		c, err := gossh.Dial("tcp", ln.Addr().String(),
			&gossh.ClientConfig{User: "x", HostKeyCallback: gossh.InsecureIgnoreHostKey()})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
}

// forward opens a reverse forward on a free port and answers with its address.
func forward(t *testing.T, c *gossh.Client) string {
	t.Helper()
	free := listen(t)
	addr := free.Addr().String()
	_ = free.Close()

	ln, err := c.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	return addr
}

func cancel(t *testing.T, c *gossh.Client, addr string) {
	t.Helper()
	host, port, _ := net.SplitHostPort(addr)
	n, _ := strconv.Atoi(port)
	if _, _, err := c.SendRequest("cancel-tcpip-forward", true,
		gossh.Marshal(&remoteForwardCancelRequest{host, uint32(n)})); err != nil {
		t.Fatal(err)
	}
}

// A cancel names a forward by address alone, and one Forwards serves every
// connection, so the address has to be looked up among THIS connection's
// forwards. Looked up among everybody's, any account could take down another
// account's NFS export by naming its port.
func TestACancelFromAnotherConnectionClosesNothing(t *testing.T) {
	dial := serveForwards(t, &Forwards{Reverse: &reserving{}})
	owner, stranger := dial(), dial()
	addr := forward(t, owner)

	cancel(t, stranger, addr)

	time.Sleep(100 * time.Millisecond)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("another connection's cancel took the forward down: %v", err)
	}
	_ = conn.Close()
}

// A forward its own connection cancels gives its reservation back then, not
// when the connection ends, or asking for the same port again is refused as
// held by another session.
func TestACancelledForwardReleasesItsReservation(t *testing.T) {
	r := &reserving{}
	dial := serveForwards(t, &Forwards{Reverse: r})
	c := dial()
	addr := forward(t, c)

	cancel(t, c, addr)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		n := len(r.released)
		r.mu.Unlock()
		if n == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("a cancelled forward kept its reservation for as long as the connection lived")
}
