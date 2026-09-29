//go:build windows

package proxy

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"

	"github.com/lhns/remote-docker/client/internal/endpointtest"
)

type closeWriter interface{ CloseWrite() error }

// go-winio half-closes only a message-mode pipe. Without it the CLI's
// CloseWrite is a silent no-op and `echo hi | docker exec -i c cat` never
// sees stdin end.
func TestEndpointPipeHalfCloses(t *testing.T) {
	endpoint := endpointtest.Endpoint(t)
	l, err := Listen(endpoint)
	if err != nil {
		t.Fatalf("binding the endpoint: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			close(accepted)
			return
		}
		accepted <- c
	}()

	timeout := 5 * time.Second
	client, err := winio.DialPipe(endpoint, &timeout)
	if err != nil {
		t.Fatalf("dialling the endpoint: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	server, ok := <-accepted
	if !ok {
		t.Fatal("accept failed")
	}
	t.Cleanup(func() { _ = server.Close() })

	t.Run("client to server", func(t *testing.T) { halfClose(t, client, server) })
	t.Run("server to client", func(t *testing.T) { halfClose(t, server, client) })
}

func halfClose(t *testing.T, from, to net.Conn) {
	t.Helper()
	cw, ok := from.(closeWriter)
	if !ok {
		t.Fatalf("%T has no CloseWrite: the pipe is not in message mode", from)
	}
	// Read concurrently, as the proxy does: CloseWrite flushes, which waits
	// for the peer to drain the pipe.
	_ = to.SetReadDeadline(time.Now().Add(5 * time.Second))
	type result struct {
		got []byte
		err error
	}
	read := make(chan result, 1)
	go func() {
		got, err := io.ReadAll(to)
		read <- result{got, err}
	}()

	if _, err := from.Write([]byte("hi")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := cw.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	r := <-read
	got, err := r.got, r.err
	if err != nil {
		t.Fatalf("read %q, then %v rather than EOF", got, err)
	}
	if string(got) != "hi" {
		t.Fatalf("read %q, want %q", got, "hi")
	}
}
