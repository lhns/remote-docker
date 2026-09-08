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
// direction before it is closed (ADR 0038).
//
// Two minutes because the workloads this carries are request/response shaped:
// a resolver, syslog, metrics. A gap that long means the exchange is over, and
// a sender that speaks again simply gets a new flow, which costs one channel
// open.
const udpFlowIdle = 2 * time.Minute

// udpFlowSweepInterval matches shareReconcileInterval and the port manager's,
// for the same reason: a cheap periodic pass, cheap enough that its cadence is
// not worth a second number.
const udpFlowSweepInterval = 30 * time.Second

// udpForward carries datagrams that arrive on a local port to a published UDP
// port inside the workspace (ADR 0038).
//
// One flow per SOURCE ADDRESS, because that is what makes a reply routable: the
// workspace's socket is connected to the container's port, so everything that
// comes back on it belongs to exactly one local sender.
//
// A flow ends when the forward does, when its channel fails, or when it has
// been idle for udpFlowIdle. The last of those is what keeps a sender whose
// source port changes per datagram, which is what a resolver does, from
// leaving a channel behind per datagram.
type udpForward struct {
	conn   net.PacketConn
	remote string
	dial   dialDatagrams

	// idle and now are fields rather than the constants above so a test can
	// expire a flow without waiting for one.
	idle time.Duration
	now  func() time.Time

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
		idle:   udpFlowIdle,
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
			continue
		}
		f.touch(flow)
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
		if _, err := f.conn.WriteTo(buf[:n], to); err != nil {
			return
		}
		f.touch(flow)
	}
}

func (f *udpForward) touch(flow *udpFlow) {
	f.mu.Lock()
	flow.lastUsed = f.now()
	f.mu.Unlock()
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

// sweepLoop expires quiet flows on the same cadence the shares and the ports
// are reconciled on.
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

// sweep closes every flow that has carried nothing for f.idle.
//
// The gate's discipline (gate.go's sweep): the scan is under the lock, the
// close is outside it, and membership is checked AGAIN under the lock before
// the flow goes, because a datagram may have arrived in between and drop
// refuses a flow the map no longer holds.
func (f *udpForward) sweep() {
	cutoff := f.now().Add(-f.idle)

	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	var expired []string
	for key, flow := range f.flows {
		if flow.lastUsed.Before(cutoff) {
			expired = append(expired, key)
		}
	}
	f.mu.Unlock()

	for _, key := range expired {
		f.expire(key, cutoff)
	}
}

func (f *udpForward) expire(key string, cutoff time.Time) {
	f.mu.Lock()
	flow, ok := f.flows[key]
	if !ok || !flow.lastUsed.Before(cutoff) {
		f.mu.Unlock()
		return
	}
	delete(f.flows, key)
	f.mu.Unlock()

	// Closing the channel is what ends the replies goroutine: its Read fails.
	_ = flow.ch.Close()
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
