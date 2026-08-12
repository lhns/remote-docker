// Package tunnel is what the client and the agent must agree about: one SSH
// connection carrying Docker API streams, an NFS export, port forwards and
// change notifications.
//
// It holds the agreements and nothing platform-specific. The two
// implementations live with the ends that run them --
// core-client/tunnelclient dials, core-agent/tunnelserver serves -- and this
// package must import neither SSH library, so that the client never links a
// server it does not run.
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
		CloseWriteOrClose(b)
	})
	wg.Go(func() {
		_, _ = io.Copy(a, b)
		CloseWriteOrClose(a)
	})
	wg.Wait()
}

// There are exactly TWO answers to a stream that cannot half-close, and which
// is right depends on what the stream carries. Both are exported, because
// writing either out by hand is how they drifted apart the first time: they
// ended up opposite, and each looked obviously correct where it stood.
//
// Choose by asking whether this connection still owes anybody anything:
//
//   - CloseWrite, while the other direction may still deliver. Nothing is
//     closed. The peer may wait out its own timeout, which is bounded; output
//     already produced and then discarded is not (ADR 0005). This is the
//     upstream half of a Docker API stream, and it is the safe default.
//   - CloseWriteOrClose, once the exchange is finished: a port forward whose
//     peer would otherwise sit in Read forever holding both connections, or a
//     response fully delivered to a client that cannot half-close and must
//     still be told there is no more.
//
// If it is not obvious which applies, it is the first.

// CloseWrite signals end-of-input without ending the connection, and leaves a
// stream that cannot do so alone. See the note above.
func CloseWrite(v any) {
	if cw, ok := v.(WriteCloser); ok {
		_ = cw.CloseWrite()
	}
}

// CloseWriteOrClose signals end-of-input, and closes a connection that cannot.
// Only for streams carrying no output back. See the note above.
func CloseWriteOrClose(c net.Conn) {
	if cw, ok := c.(WriteCloser); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}
