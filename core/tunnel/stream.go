// Package tunnel is what the client and the agent must agree about: one SSH
// connection carrying Docker API streams, an NFS export, port forwards and
// change notifications.
//
// It imports neither SSH library; core-client/tunnelclient dials and
// core-agent/tunnelserver serves. This file is the bidirectional copy and what
// half-closing means, which both ends must answer identically: two copies of it
// once disagreed about a connection that cannot half-close, and `docker run`
// exited 0 having printed nothing.
package tunnel

import (
	"io"
	"net"
	"sync"
)

// WriteCloser is the half-close an SSH channel, a unix socket and a TCP
// connection all have, and a plain io.ReadWriter does not.
type WriteCloser interface{ CloseWrite() error }

// Splice copies between two streams until either ends, signalling end-of-input
// in BOTH directions. Without the second signal a container exiting leaves each
// side waiting on the other for ~90 seconds. That only shows where the peer
// cannot half-close, which is a Windows named pipe, the Docker API endpoint of
// the Windows client.
func Splice(a, b io.ReadWriter) {
	var wg sync.WaitGroup
	wg.Go(func() {
		_, _ = io.Copy(b, a)
		CloseWrite(b)
	})
	wg.Go(func() {
		_, _ = io.Copy(a, b)
		CloseWrite(a)
	})
	wg.Wait()
}

// SpliceAndClose is Splice for a stream carrying no output back, such as a port
// forward: one that cannot half-close is closed rather than left in Read
// forever. Never use it for a Docker API stream, whose closing drops the
// container's output (ADR 0005).
func SpliceAndClose(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Go(func() {
		_, _ = io.Copy(b, a)
		CloseWriteOrClose(b)
	})
	wg.Go(func() {
		_, _ = io.Copy(a, b)
		CloseWriteOrClose(a)
	})
	wg.Wait()
}

// CloseWrite signals end-of-input and leaves a stream that cannot alone: the
// peer waits out its own bounded timeout, where discarded output is lost for
// good. The default when unsure.
func CloseWrite(v any) {
	if cw, ok := v.(WriteCloser); ok {
		_ = cw.CloseWrite()
	}
}

// CloseWriteOrClose signals end-of-input and closes a connection that cannot.
// Only once the exchange is finished: a port forward, or a response fully
// delivered to a client that must still be told there is no more.
func CloseWriteOrClose(c net.Conn) {
	if cw, ok := c.(WriteCloser); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}
