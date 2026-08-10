package sshx

import (
	"fmt"
	"net"
	"sync"

	"github.com/lhns/remote-docker/internal/iox"
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
			iox.SpliceAndClose(local, remote)
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
