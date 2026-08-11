// Package tunnel is what the client and the agent must agree about: one SSH
// connection carrying Docker API streams, an NFS export, port forwards and
// change notifications.
//
// It holds the agreements and nothing platform-specific. `tunnel/client` dials
// and `tunnel/server` serves; this package must import neither SSH library, so
// that the client never links a server it does not run.
//
// This file is the first agreement, and the reason the package exists. The
// bidirectional copy and the meaning of half-closing lived in both binaries,
// and the copies had OPPOSITE fallbacks for a connection that cannot
// half-close: one did nothing, the other closed the whole connection, which the
// project's own invariant forbids. It failed as `docker run` exiting 0 having
// printed nothing.
package tunnel

import (
	"io"
	"net"
	"sync"
)

// WriteCloser is the half-close an SSH channel, a unix socket and a TCP
// connection all have, and a plain io.ReadWriter does not.
type WriteCloser interface{ CloseWrite() error }

// Splice copies between two streams until either ends, signalling
// end-of-input in BOTH directions.
//
// The second signal is not symmetry for its own sake. Without it, a copy that
// ends because the container exited says nothing, so the peer waits on a
// stream that will never speak again while this side waits for the peer: a
// circular wait broken only by a ~90 second timeout.
//
// Only load-bearing when the peer cannot half-close, which is why it survived
// CI. A unix socket can; a Windows named pipe cannot, and named pipes are
// what the Windows client serves the Docker API on.
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

// SpliceAndClose is Splice for a stream that must not be allowed to hang: one
// that cannot half-close is closed rather than left alone.
//
// That is right here and wrong for Splice, which is why both exist and why
// neither should be "simplified" into the other. A Docker API stream must
// never be closed when one direction ends, because the stream being closed is
// the one carrying the container's output back (ADR 0005). A port forward
// carries no such stream, so a direction that cannot signal end-of-input
// leaves the other in Read forever, holding both connections.
//
// The fallback does not fire on the forward path today: both ends are
// net.Conn, TCP has CloseWrite, and ssh's channel-backed conn promotes it. It
// is here for when that stops being true.
func SpliceAndClose(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Go(func() {
		_, _ = io.Copy(b, a)
		closeWriteOrClose(b)
	})
	wg.Go(func() {
		_, _ = io.Copy(a, b)
		closeWriteOrClose(a)
	})
	wg.Wait()
}

func closeWriteOrClose(c net.Conn) {
	if cw, ok := c.(WriteCloser); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}

// CloseWrite signals end-of-input without ending the connection.
//
// A stream that cannot half-close is left alone. Closing it instead is the
// obvious-looking alternative and it is the failure ADR 0005 records: the
// stream closed is the one carrying the container's output back. A peer that
// never learns this side is finished waits for its own timeout, which is
// bounded; output already produced and then lost is not recoverable.
func CloseWrite(v any) {
	if cw, ok := v.(WriteCloser); ok {
		_ = cw.CloseWrite()
	}
}
