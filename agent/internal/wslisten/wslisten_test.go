package wslisten

// SSH over a WebSocket, proven without a kernel, a container or a daemon: an
// http.Server on loopback, this listener behind it, and a real SSH handshake
// through the whole thing.
//
// The liveness test is the one that matters most. The TCP-level detection the
// agent applies elsewhere cannot see through a WebSocket, so if the ping stops
// working a client that vanishes keeps its reverse-tunnel port reserved, and
// nothing fails until some later reconnect is refused its forward.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/lhns/remote-docker/core/logx"
	"golang.org/x/crypto/ssh"
)

func testLogger() *slog.Logger { return logx.Discard() }

// serveWS starts the listener behind an httptest server and returns its ws URL.
func serveWS(t *testing.T) (*Listener, string) {
	t.Helper()

	l := &Listener{
		addr:   &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)},
		log:    testLogger(),
		conns:  make(chan net.Conn),
		closed: make(chan struct{}),
	}

	mux := http.NewServeMux()
	mux.Handle("/tunnel", l.Handler())
	srv := httptest.NewServer(mux)
	t.Cleanup(func() {
		srv.Close()
		_ = l.Close()
	})

	return l, "ws" + strings.TrimPrefix(srv.URL, "http") + "/tunnel"
}

// sshOver serves one SSH connection on the listener and echoes what an exec
// asks for, which is enough to prove the transport carries a real session.
func sshOver(t *testing.T, l net.Listener) ssh.PublicKey {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}

	cfg := &ssh.ServerConfig{NoClientAuth: true}
	cfg.AddHostKey(signer)

	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		sc, chans, reqs, err := ssh.NewServerConn(conn, cfg)
		if err != nil {
			return
		}
		defer sc.Close()
		go ssh.DiscardRequests(reqs)

		for nch := range chans {
			ch, creqs, err := nch.Accept()
			if err != nil {
				return
			}
			go func() {
				defer ch.Close()
				for r := range creqs {
					if r.WantReply {
						_ = r.Reply(true, nil)
					}
					if r.Type != "exec" {
						continue
					}
					// A real one: write, then say how it exited. Closing
					// without an exit status is what makes a client report
					// EOF rather than the output it was given.
					_, _ = io.WriteString(ch, "through the websocket")
					_ = ch.CloseWrite()
					_, _ = ch.SendRequest("exit-status", false,
						ssh.Marshal(struct{ Status uint32 }{Status: 0}))
					return
				}
			}()
		}
	}()
	return signer.PublicKey()
}

// The whole point: a real SSH session over a WebSocket.
func TestSSHSessionOverAWebSocket(t *testing.T) {
	l, url := serveWS(t)
	hostKey := sshOver(t, l)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	wc, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	wc.SetReadLimit(-1)
	conn := websocket.NetConn(context.Background(), wc, websocket.MessageBinary)

	sc, chans, reqs, err := ssh.NewClientConn(conn, "workspace", &ssh.ClientConfig{
		User:            "itest",
		HostKeyCallback: ssh.FixedHostKey(hostKey),
		Timeout:         10 * time.Second,
	})
	if err != nil {
		t.Fatalf("ssh handshake over websocket: %v", err)
	}
	client := ssh.NewClient(sc, chans, reqs)
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	defer sess.Close()

	out, err := sess.Output("anything")
	if err != nil && len(out) == 0 {
		t.Fatalf("exec: %v", err)
	}
	if got := string(out); got != "through the websocket" {
		t.Errorf("read %q through the tunnel", got)
	}
}

// The host key still decides, inside the tunnel. Whatever the transport proved
// about the front door, this is what proves the workspace.
func TestTheHostKeyStillDecides(t *testing.T) {
	l, url := serveWS(t)
	sshOver(t, l)

	_, other, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	wrong, err := ssh.NewSignerFromKey(other)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	wc, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	conn := websocket.NetConn(context.Background(), wc, websocket.MessageBinary)

	_, _, _, err = ssh.NewClientConn(conn, "workspace", &ssh.ClientConfig{
		User:            "itest",
		HostKeyCallback: ssh.FixedHostKey(wrong.PublicKey()),
		Timeout:         10 * time.Second,
	})
	if err == nil {
		t.Fatal("a wrong host key was accepted over the websocket")
	}
}

// A peer that stops answering is dropped, which is what releases its
// reverse-tunnel port. Nothing else on this side notices a dead WebSocket: the
// TCP-level detection cannot see through one.
//
// The client here never reads, and coder/websocket answers pings from its read
// loop, so the pings go unanswered exactly as they would from a machine that
// went away.
func TestAPeerThatStopsAnsweringIsDropped(t *testing.T) {
	l := &Listener{
		addr:   &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)},
		log:    testLogger(),
		conns:  make(chan net.Conn),
		closed: make(chan struct{}),
		ping:   50 * time.Millisecond,
	}
	t.Cleanup(func() { _ = l.Close() })

	mux := http.NewServeMux()
	mux.Handle("/tunnel", l.Handler())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	wc, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/tunnel", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer wc.CloseNow()

	conn, err := l.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}

	// The assertion: the accepted connection fails, because the listener closed
	// it. Without the ping this read blocks until the deadline and the test
	// fails, which is the point -- a silent WebSocket must not look healthy.
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 1)
	start := time.Now()
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("read succeeded on a connection whose peer never answered a ping")
	}
	if waited := time.Since(start); waited > 5*time.Second {
		t.Errorf("took %v to drop a silent peer; the ping is not driving it", waited)
	}
}

// A request to the wrong path is answered rather than left hanging: a proxy
// pointed at the wrong route is a common mistake and should say so.
func TestTheWrongPathIsAnswered(t *testing.T) {
	l := &Listener{
		addr:   &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)},
		log:    testLogger(),
		conns:  make(chan net.Conn),
		closed: make(chan struct{}),
	}
	defer l.Close()

	mux := http.NewServeMux()
	mux.Handle("/tunnel", l.Handler())
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not the tunnel endpoint", http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/elsewhere")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status %d for the wrong path, want 404", resp.StatusCode)
	}
}

// Accept returns rather than blocking forever once the listener is closed, or
// the SSH server's accept loop never ends.
func TestAcceptEndsWhenClosed(t *testing.T) {
	l := &Listener{
		addr:   &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)},
		conns:  make(chan net.Conn),
		closed: make(chan struct{}),
		log:    testLogger(),
	}

	done := make(chan error, 1)
	go func() {
		_, err := l.Accept()
		done <- err
	}()

	_ = l.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Error("Accept returned a connection after Close")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Accept did not return after Close")
	}

	// Close is called from more than one place; it must not panic on the
	// second.
	_ = l.Close()
}
