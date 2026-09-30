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

// Without message mode the CLI's half-close of stdin is a silent no-op.
func TestEndpointPipeHalfCloses(t *testing.T) {
	endpoint := endpointtest.Endpoint(t)
	l, err := Listen(endpoint)
	if err != nil {
		t.Fatalf("binding the endpoint: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := l.Accept()
		accepted <- c
	}()

	timeout := 5 * time.Second
	client, err := winio.DialPipe(endpoint, &timeout)
	if err != nil {
		t.Fatalf("dialling the endpoint: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	server := <-accepted
	if server == nil {
		t.Fatal("accept failed")
	}
	t.Cleanup(func() { _ = server.Close() })

	t.Run("client to server", func(t *testing.T) { halfClose(t, client, server) })
	t.Run("server to client", func(t *testing.T) { halfClose(t, server, client) })
}

func halfClose(t *testing.T, from, to net.Conn) {
	cw, ok := from.(interface{ CloseWrite() error })
	if !ok {
		t.Fatalf("%T has no CloseWrite: the pipe is not in message mode", from)
	}
	// Read concurrently: go-winio's CloseWrite blocks until the peer drains.
	_ = to.SetReadDeadline(time.Now().Add(5 * time.Second))
	var got []byte
	read := make(chan error, 1)
	go func() {
		var err error
		got, err = io.ReadAll(to)
		read <- err
	}()

	if _, err := from.Write([]byte("hi")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := cw.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	if err := <-read; err != nil || string(got) != "hi" {
		t.Fatalf("read %q, %v; want %q, EOF", got, err, "hi")
	}
}
