package session

import (
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/lhns/remote-docker/core/tunnel"
)

// dialDatagrams opens one datagram flow to an address inside the workspace.
// An interface so the forward below can be tested without a workspace.
type dialDatagrams func(remoteAddr string) (io.ReadWriteCloser, error)

// udpFlowIdle is how long a flow may go without carrying a datagram in either
// direction before it is closed, and udpFlowSweepInterval how often that is
// looked for. Both are reasoned about in ADR 0038.
const (
	udpFlowIdle          = 2 * time.Minute
	udpFlowSweepInterval = 30 * time.Second
)

// udpForward carries datagrams that arrive on a local port to a published UDP
// port inside the workspace (ADR 0038).
//
// One flow per SOURCE ADDRESS, because that is what makes a reply routable: the
// workspace's socket is connected to the container's port, so everything that
// comes back on it belongs to exactly one local sender.
//
// A flow ends with the forward, with its channel, or after udpFlowIdle without
// a datagram. The last of those is what keeps a sender whose source port
// changes per datagram, a resolver being one, from leaving a channel behind
// per datagram.
type udpForward struct {
	conn   net.PacketConn
	remote string
	dial   dialDatagrams

	// now is a field so a test can reach udpFlowIdle without waiting for it.
	now func() time.Time

	mu     sync.Mutex
	flows  map[string]*udpFlow
	closed bool

	done chan struct{}
	wg   sync.WaitGroup
}

// udpFlow is one sender's channel and when it last carried a datagram.
// lastUsed is guarded by udpForward.mu.
type udpFlow struct {
	ch       io.ReadWriteCloser
	lastUsed time.Time
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
		now:    time.Now,
		flows:  map[string]*udpFlow{},
		done:   make(chan struct{}),
	}
	f.wg.Add(2)
	go f.serve()
	go f.sweepLoop()
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
		if _, err := flow.ch.Write(buf[:n]); err != nil {
			f.drop(from.String(), flow)
		}
	}
}

// flowFor is the flow belonging to a sender, opening one the first time it is
// seen.
func (f *udpForward) flowFor(from net.Addr) (*udpFlow, error) {
	key := from.String()

	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil, net.ErrClosed
	}
	if flow, ok := f.flows[key]; ok {
		flow.lastUsed = f.now()
		f.mu.Unlock()
		return flow, nil
	}
	f.mu.Unlock()

	ch, err := f.dial(f.remote)
	if err != nil {
		return nil, err
	}

	f.mu.Lock()
	// Somebody else may have opened one while this was dialling, and two flows
	// for one sender would split its replies between them.
	if existing, ok := f.flows[key]; ok {
		existing.lastUsed = f.now()
		f.mu.Unlock()
		_ = ch.Close()
		return existing, nil
	}
	if f.closed {
		f.mu.Unlock()
		_ = ch.Close()
		return nil, net.ErrClosed
	}
	flow := &udpFlow{ch: ch, lastUsed: f.now()}
	f.flows[key] = flow
	f.mu.Unlock()

	f.wg.Add(1)
	go f.replies(from, flow)
	return flow, nil
}

// replies carries what the container sends back to the sender it belongs to.
func (f *udpForward) replies(to net.Addr, flow *udpFlow) {
	defer f.wg.Done()
	defer f.drop(to.String(), flow)

	buf := make([]byte, tunnel.MaxDatagram)
	for {
		n, err := flow.ch.Read(buf)
		if err != nil {
			return
		}
		// A reply is use as much as a datagram sent: flowFor stamps the one
		// direction, this stamps the other.
		f.mu.Lock()
		flow.lastUsed = f.now()
		f.mu.Unlock()

		if _, err := f.conn.WriteTo(buf[:n], to); err != nil {
			return
		}
	}
}

// drop removes a flow and closes it, but only while the map still holds THIS
// flow. A sweep may have expired it already and the sender may have opened
// another under the same key, which dropping by name alone would close.
func (f *udpForward) drop(key string, flow *udpFlow) {
	f.mu.Lock()
	if current, ok := f.flows[key]; !ok || current != flow {
		f.mu.Unlock()
		return
	}
	delete(f.flows, key)
	f.mu.Unlock()

	_ = flow.ch.Close()
}

// sweepLoop expires quiet flows.
func (f *udpForward) sweepLoop() {
	defer f.wg.Done()

	t := time.NewTicker(udpFlowSweepInterval)
	defer t.Stop()
	for {
		select {
		case <-f.done:
			return
		case <-t.C:
			f.sweep()
		}
	}
}

// sweep closes every flow that has carried nothing for udpFlowIdle. The
// selection and the removal are one critical section, as gate.go's sweep is;
// only the closes are outside the lock, and closing a channel is what ends the
// replies goroutine reading it.
func (f *udpForward) sweep() {
	cutoff := f.now().Add(-udpFlowIdle)

	f.mu.Lock()
	var expired []*udpFlow
	for key, flow := range f.flows {
		if flow.lastUsed.Before(cutoff) {
			expired = append(expired, flow)
			delete(f.flows, key)
		}
	}
	f.mu.Unlock()

	for _, flow := range expired {
		_ = flow.ch.Close()
	}
}

// Close ends the forward and every flow under it.
func (f *udpForward) Close() error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	f.closed = true
	flows := f.flows
	f.flows = map[string]*udpFlow{}
	close(f.done)
	f.mu.Unlock()

	err := f.conn.Close()
	for _, flow := range flows {
		_ = flow.ch.Close()
	}
	f.wg.Wait()
	return err
}
