package sshx

import (
	"fmt"
	"io"
	"net"
	"sync"
)

// Forward is a local listener whose connections are carried to an address on
// the workspace. This is ssh -L, and it is how a published container port
// becomes reachable at the same address on the client machine.
type Forward struct {
	// Local is the address actually bound, which is authoritative: asking for
	// port 0 gets a kernel-chosen port, and callers must report what was
	// bound rather than what was requested.
	Local  net.Addr
	Remote string

	listener net.Listener
	closed   chan struct{}
	once     sync.Once
	wg       sync.WaitGroup
}

// Forward binds localAddr and carries every connection to remoteAddr on the
// workspace.
//
// A port already in use on this machine fails here, and that failure must be
// surfaced rather than retried on a different port: a listener at an address
// nobody asked for looks like success and breaks the next thing that expects
// the real one.
func (c *Client) Forward(localAddr, remoteAddr string) (*Forward, error) {
	l, err := net.Listen("tcp", localAddr)
	if err != nil {
		return nil, fmt.Errorf("sshx: binding %s: %w", localAddr, err)
	}

	f := &Forward{
		Local:    l.Addr(),
		Remote:   remoteAddr,
		listener: l,
		closed:   make(chan struct{}),
	}

	f.wg.Go(func() { f.accept(c) })
	return f, nil
}

func (f *Forward) accept(c *Client) {
	for {
		local, err := f.listener.Accept()
		if err != nil {
			select {
			case <-f.closed:
				return
			default:
			}
			// A transient accept error should not silently kill the forward,
			// but a listener that is genuinely broken would spin here, so
			// stop rather than loop.
			return
		}

		f.wg.Go(func() {
			defer local.Close()

			remote, err := c.DialRemote(f.Remote)
			if err != nil {
				return
			}
			defer remote.Close()
			pipe(local, remote)
		})
	}
}

// Close stops accepting and waits for connections in flight to finish.
func (f *Forward) Close() error {
	var err error
	f.once.Do(func() {
		close(f.closed)
		err = f.listener.Close()
	})
	f.wg.Wait()
	return err
}

// Serve accepts on l -- typically a listener obtained from Client.Listen, so
// running on the workspace -- and carries each connection to a local address.
// This is the reverse direction, used when something on the workspace must
// reach a service here.
func Serve(l net.Listener, dial func() (net.Conn, error)) error {
	for {
		remote, err := l.Accept()
		if err != nil {
			return err
		}
		go func() {
			defer remote.Close()
			local, err := dial()
			if err != nil {
				return
			}
			defer local.Close()
			pipe(remote, local)
		}()
	}
}

// pipe copies in both directions until either side ends, then unblocks the
// other by closing both. Without the close, one direction can sit in Read
// forever after its peer has gone.
func pipe(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Go(func() {
		_, _ = io.Copy(a, b)
		closeWrite(a)
	})
	wg.Go(func() {
		_, _ = io.Copy(b, a)
		closeWrite(b)
	})
	wg.Wait()
}

// closeWrite half-closes where the connection supports it, so the peer sees a
// clean EOF rather than a reset. SSH channels and TCP both do.
func closeWrite(c net.Conn) {
	type writeCloser interface{ CloseWrite() error }
	if wc, ok := c.(writeCloser); ok {
		_ = wc.CloseWrite()
		return
	}
	c.Close()
}
