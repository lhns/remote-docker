package session

import (
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/lhns/remote-docker/core/tunnel"
)

// dialDatagrams opens one datagram flow to an address inside the workspace.
// An interface so the forward below can be tested without a workspace.
type dialDatagrams func(remoteAddr string) (io.ReadWriteCloser, error)

// udpForward carries datagrams that arrive on a local port to a published UDP
// port inside the workspace (ADR 0038).
//
// One flow per SOURCE ADDRESS, because that is what makes a reply routable: the
// workspace's socket is connected to the container's port, so everything that
// comes back on it belongs to exactly one local sender.
//
// A flow lives as long as this forward, which is the rule a TCP forward already
// follows: it ends when the container stops or stops publishing the port. What
// that costs a sender whose source port changes per datagram is in ADR 0038.
type udpForward struct {
	conn   net.PacketConn
	remote string
	dial   dialDatagrams

	mu     sync.Mutex
	flows  map[string]io.ReadWriteCloser
	closed bool

	wg sync.WaitGroup
}

func newUDPForward(localAddr, remoteAddr string, dial dialDatagrams) (*udpForward, error) {
	conn, err := net.ListenPacket("udp", localAddr)
	if err != nil {
		return nil, fmt.Errorf("tunnel: listening on %s: %w", localAddr, err)
	}

	f := &udpForward{
		conn:   conn,
		remote: remoteAddr,
		dial:   dial,
		flows:  map[string]io.ReadWriteCloser{},
	}
	f.wg.Add(1)
	go f.serve()
	return f, nil
}

func (f *udpForward) LocalAddr() net.Addr { return f.conn.LocalAddr() }

// serve reads from the local socket forever, which is the only way to learn
// that a new sender exists: a datagram socket has no accept.
func (f *udpForward) serve() {
	defer f.wg.Done()

	buf := make([]byte, tunnel.MaxDatagram)
	for {
		n, from, err := f.conn.ReadFrom(buf)
		if err != nil {
			return
		}

		flow, err := f.flowFor(from)
		if err != nil {
			// Reported by the caller's logger, not here: one workspace that
			// cannot carry datagrams must not fill a log with one line per
			// datagram.
			continue
		}
		if _, err := flow.Write(buf[:n]); err != nil {
			f.drop(from.String())
		}
	}
}

// flowFor is the flow belonging to a sender, opening one the first time it is
// seen.
func (f *udpForward) flowFor(from net.Addr) (io.ReadWriteCloser, error) {
	key := from.String()

	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil, net.ErrClosed
	}
	if flow, ok := f.flows[key]; ok {
		f.mu.Unlock()
		return flow, nil
	}
	f.mu.Unlock()

	flow, err := f.dial(f.remote)
	if err != nil {
		return nil, err
	}

	f.mu.Lock()
	// Somebody else may have opened one while this was dialling, and two flows
	// for one sender would split its replies between them.
	if existing, ok := f.flows[key]; ok {
		f.mu.Unlock()
		_ = flow.Close()
		return existing, nil
	}
	if f.closed {
		f.mu.Unlock()
		_ = flow.Close()
		return nil, net.ErrClosed
	}
	f.flows[key] = flow
	f.mu.Unlock()

	f.wg.Add(1)
	go f.replies(from, flow)
	return flow, nil
}

// replies carries what the container sends back to the sender it belongs to.
func (f *udpForward) replies(to net.Addr, flow io.ReadWriteCloser) {
	defer f.wg.Done()
	defer f.drop(to.String())

	buf := make([]byte, tunnel.MaxDatagram)
	for {
		n, err := flow.Read(buf)
		if err != nil {
			return
		}
		if _, err := f.conn.WriteTo(buf[:n], to); err != nil {
			return
		}
	}
}

func (f *udpForward) drop(key string) {
	f.mu.Lock()
	flow, ok := f.flows[key]
	delete(f.flows, key)
	f.mu.Unlock()

	if ok {
		_ = flow.Close()
	}
}

// Close ends the forward and every flow under it, which is the whole lifetime
// rule: they go when the forward goes.
func (f *udpForward) Close() error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	f.closed = true
	flows := f.flows
	f.flows = map[string]io.ReadWriteCloser{}
	f.mu.Unlock()

	err := f.conn.Close()
	for _, flow := range flows {
		_ = flow.Close()
	}
	f.wg.Wait()
	return err
}
