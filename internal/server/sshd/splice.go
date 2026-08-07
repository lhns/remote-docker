package sshd

import (
	"io"
	"sync"
)

// Deliberately not in session.go, which is build-tagged Linux for
// syscall.Credential and the pty. Nothing here is platform-specific, and a
// bug that lived in it was invisible to CI for exactly that reason -- the
// tests could not run on the machine it was written on.

// splice copies between a session and a connection until either ends.
func splice(a io.ReadWriter, b io.ReadWriteCloser) {
	var wg sync.WaitGroup
	wg.Go(func() {
		_, _ = io.Copy(b, a)
		closeWrite(b)
	})
	wg.Go(func() {
		_, _ = io.Copy(a, b)
		// Both directions signal end-of-input, and the second one is not
		// symmetry for its own sake -- leaving it out deadlocks.
		//
		// This copy ends when the daemon closes: the container exited, the
		// attach is over. Saying nothing left the client waiting on a stream
		// that would never speak again, while THIS side waited for the client
		// to half-close its own half -- a circular wait broken only by a
		// ~90 second timeout. `docker run` took a minute and a half to return
		// from a container that had finished in one second.
		//
		// It survived CI because the missing signal is only load-bearing when
		// the client cannot half-close. A unix socket can, so the Linux client
		// unwound it every time; a Windows named pipe cannot, and named pipes
		// are what the Windows client serves the Docker API on.
		closeWrite(a)
	})
	wg.Wait()
}

// closeWrite signals end-of-input without ending the connection, where the
// other side supports it. A gliderlabs Session embeds gossh.Channel, so an SSH
// session does; a unix socket does; a plain io.ReadWriter does not.
func closeWrite(v any) {
	if cw, ok := v.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}
