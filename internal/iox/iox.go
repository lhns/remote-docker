// Package iox is the one bidirectional copy, and the one answer to what
// half-closing means.
//
// It is shared because the two binaries are the two ends of the same stream.
// They each had their own copy, and the copies had OPPOSITE fallbacks for a
// connection that cannot half-close: the agent's did nothing, the client's
// closed the whole connection -- which is what the project's own invariant
// forbids in as many words:
//
//	Half-close the upstream, never close it. `docker run` without -i closes
//	its stdin as soon as attach is established; closing the whole stream in
//	response tears down the session carrying the container's output.
//
// The client's fallback could not fire, because the path it is on is TCP at
// both ends and TCP has CloseWrite. That it could DIFFER at all is the reason
// this package exists: the knowledge that one direction's missing signal cost
// ninety seconds per `docker run` lived in one copy and not the other.
package iox

import (
	"io"
	"net"
	"sync"
)

// WriteCloser is the half-close an SSH channel, a unix socket and a TCP
// connection all have, and a plain io.ReadWriter does not.
type WriteCloser interface{ CloseWrite() error }

// Splice copies between two streams until either ends, signalling end-of-input
// in BOTH directions.
//
// The second signal is not symmetry for its own sake -- leaving it out
// deadlocks. That copy ends when the far side closes: the container exited,
// the attach is over. Saying nothing leaves the peer waiting on a stream that
// will never speak again while this side waits for the peer to half-close its
// own half. A circular wait, broken only by a ~90 second timeout, so
// `docker run` took a minute and a half to return from a container that had
// finished in one second.
//
// It survived CI because the missing signal is only load-bearing when the peer
// cannot half-close. A unix socket can, so the Linux client unwound it every
// time; a Windows named pipe cannot, and named pipes are what the Windows
// client serves the Docker API on.
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

// SpliceAndClose is Splice for a stream that must not be allowed to hang.
//
// Same half-close, with one difference: a stream that cannot half-close is
// CLOSED rather than left alone. That is right here and wrong for Splice, and
// the difference is the reason both live in this file instead of being one
// function somebody later "simplifies".
//
//   - A Docker API stream must never be closed in response to one direction
//     ending. The stream being closed is the one carrying the container's
//     output back (ADR 0005), so the cost of doing nothing -- a peer that
//     waits for its own timeout -- is smaller than the cost of acting.
//   - A port forward carries no such stream. If one direction cannot signal
//     end-of-input, the other sits in Read forever, and the goroutine and the
//     two connections behind it are never released.
//
// In practice the fallback does not fire on the forward path either: both ends
// are net.Conn, TCP has CloseWrite, and x/crypto/ssh's channel-backed conn
// embeds ssh.Channel and so promotes it. It is here for the case that stops
// being true, and the caller says which behaviour it wants rather than finding
// out.
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
// A stream that cannot half-close is left ALONE, and that is the decision this
// package was made to have in one place. Closing it instead would be the
// obvious-looking alternative and it is the failure ADR 0005 records: the
// stream being closed is the one carrying the container's output back.
//
// The cost of doing nothing is bounded and known -- a peer that never learns
// this side is finished waits for its own timeout -- and it is smaller than
// losing output that has already been produced.
func CloseWrite(v any) {
	if cw, ok := v.(WriteCloser); ok {
		_ = cw.CloseWrite()
	}
}
