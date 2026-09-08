package session

import (
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// fakeFlow stands in for one datagram channel: what reached the workspace, and
// a way to answer as the container would.
type fakeFlow struct {
	mu      sync.Mutex
	written [][]byte
	closed  bool

	replies chan []byte
}

func newFakeFlow() *fakeFlow {
	return &fakeFlow{replies: make(chan []byte, 8)}
}

func (f *fakeFlow) Read(p []byte) (int, error) {
	payload, ok := <-f.replies
	if !ok {
		return 0, net.ErrClosed
	}
	return copy(p, payload), nil
}

func (f *fakeFlow) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, net.ErrClosed
	}
	f.written = append(f.written, append([]byte(nil), p...))
	return len(p), nil
}

func (f *fakeFlow) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.replies)
	}
	return nil
}

func (f *fakeFlow) sent() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.written...)
}

func (f *fakeFlow) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// dialer hands out fake flows and keeps them, so a test can answer as the
// container and count how many were opened.
type dialer struct {
	mu    sync.Mutex
	flows []*fakeFlow
	err   error
}

func (d *dialer) dial(string) (io.ReadWriteCloser, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err != nil {
		return nil, d.err
	}
	f := newFakeFlow()
	d.flows = append(d.flows, f)
	return f, nil
}

func (d *dialer) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.flows)
}

func (d *dialer) flow(i int) *fakeFlow {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.flows[i]
}

// waitFor gives a goroutine a bounded chance to get somewhere, so a failure is
// a failure rather than a race.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for range 200 {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// senderTo is a local UDP socket talking to the forward.
func senderTo(t *testing.T, fwd *udpForward) net.Conn {
	t.Helper()
	c, err := net.Dial("udp", fwd.LocalAddr().String())
	if err != nil {
		t.Fatalf("dialling the forward: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func forwardTo(t *testing.T, d *dialer) *udpForward {
	t.Helper()
	fwd, err := newUDPForward("127.0.0.1:0", "127.0.0.1:5353", d.dial)
	if err != nil {
		t.Fatalf("newUDPForward: %v", err)
	}
	t.Cleanup(func() { _ = fwd.Close() })
	return fwd
}

func TestADatagramReachesTheWorkspace(t *testing.T) {
	d := &dialer{}
	sender := senderTo(t, forwardTo(t, d))

	if _, err := sender.Write([]byte("query")); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "the datagram to reach the flow", func() bool {
		return d.count() == 1 && len(d.flow(0).sent()) == 1
	})
	if got := string(d.flow(0).sent()[0]); got != "query" {
		t.Errorf("the workspace received %q", got)
	}
}

// A reply goes back to the sender that asked, which is the whole reason a flow
// belongs to one source address.
func TestAReplyReachesTheSender(t *testing.T) {
	d := &dialer{}
	sender := senderTo(t, forwardTo(t, d))

	if _, err := sender.Write([]byte("query")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the flow to open", func() bool { return d.count() == 1 })

	d.flow(0).replies <- []byte("answer")

	buf := make([]byte, 64)
	if err := sender.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	n, err := sender.Read(buf)
	if err != nil {
		t.Fatalf("the reply never arrived: %v", err)
	}
	if got := string(buf[:n]); got != "answer" {
		t.Errorf("the sender received %q", got)
	}
}

// Two senders get two flows, and one answer does not reach the other.
func TestTwoSendersDoNotCross(t *testing.T) {
	d := &dialer{}
	fwd := forwardTo(t, d)
	first, second := senderTo(t, fwd), senderTo(t, fwd)

	if _, err := first.Write([]byte("from first")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the first flow", func() bool { return d.count() == 1 })
	if _, err := second.Write([]byte("from second")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the second flow", func() bool { return d.count() == 2 })

	// The flow opened first belongs to the sender that wrote first.
	d.flow(0).replies <- []byte("for first")

	buf := make([]byte, 64)
	if err := first.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Read(buf); err != nil {
		t.Fatalf("the sender that asked got nothing: %v", err)
	}

	if err := second.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Read(buf); err == nil {
		t.Error("a reply reached a sender it did not belong to")
	}
}

// One flow per source address, not one per datagram.
func TestOneFlowPerSender(t *testing.T) {
	d := &dialer{}
	sender := senderTo(t, forwardTo(t, d))

	for range 3 {
		if _, err := sender.Write([]byte("again")); err != nil {
			t.Fatal(err)
		}
	}

	waitFor(t, "three datagrams", func() bool {
		return d.count() == 1 && len(d.flow(0).sent()) == 3
	})
}

// A workspace too old to know the channel type refuses it. The listener stays,
// nothing is carried, and the session does not fail: that is exactly how this
// behaved before UDP was carried at all.
func TestAWorkspaceThatRefusesTheChannel(t *testing.T) {
	d := &dialer{err: errors.New("ssh: rejected: unknown channel type")}
	sender := senderTo(t, forwardTo(t, d))

	if _, err := sender.Write([]byte("query")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)

	if d.count() != 0 {
		t.Errorf("%d flows opened against a workspace that refuses them", d.count())
	}
}

// Close takes every flow with it, which is the entire lifetime rule: they end
// when the forward does, as a TCP forward already works.
func TestCloseEndsEveryFlow(t *testing.T) {
	d := &dialer{}
	fwd, err := newUDPForward("127.0.0.1:0", "127.0.0.1:5353", d.dial)
	if err != nil {
		t.Fatal(err)
	}
	sender, err := net.Dial("udp", fwd.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sender.Close() }()

	if _, err := sender.Write([]byte("query")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the flow to open", func() bool { return d.count() == 1 })

	if err := fwd.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !d.flow(0).isClosed() {
		t.Error("a flow outlived the forward")
	}
}

// countFlows is the number of flows the forward holds right now.
func (f *udpForward) countFlows() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.flows)
}

// forwardWithClock hands back a forward whose idle window the test drives, so
// nothing here waits for a real one. The returned function moves the forward's
// clock forward.
func forwardWithClock(t *testing.T, d *dialer, idle time.Duration) (*udpForward, func(time.Duration)) {
	t.Helper()
	fwd := forwardTo(t, d)

	var mu sync.Mutex
	base := time.Now()
	offset := time.Duration(0)

	// Set under the forward's own lock: serve reads now while holding it, and
	// the race detector counts a plain assignment here as a write against that.
	fwd.mu.Lock()
	fwd.idle = idle
	fwd.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return base.Add(offset)
	}
	fwd.mu.Unlock()

	return fwd, func(by time.Duration) {
		mu.Lock()
		offset += by
		mu.Unlock()
	}
}

// A flow that goes quiet is closed, which is what keeps a sender whose source
// port changes per datagram from leaving one channel behind per datagram
// (ADR 0038).
func TestAQuietFlowExpires(t *testing.T) {
	d := &dialer{}
	fwd, advance := forwardWithClock(t, d, time.Minute)
	sender := senderTo(t, fwd)

	if _, err := sender.Write([]byte("query")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the flow to open", func() bool { return d.count() == 1 })

	fwd.sweep()
	if d.flow(0).isClosed() {
		t.Fatal("a flow was closed before its idle window had passed")
	}

	advance(2 * time.Minute)
	fwd.sweep()

	if !d.flow(0).isClosed() {
		t.Error("a flow that carried nothing for twice its idle window is still open")
	}
	if n := fwd.countFlows(); n != 0 {
		t.Errorf("the forward still holds %d flows", n)
	}
}

// A flow still carrying datagrams is not expired underneath its sender.
func TestABusyFlowSurvivesASweep(t *testing.T) {
	d := &dialer{}
	fwd, advance := forwardWithClock(t, d, time.Minute)
	sender := senderTo(t, fwd)

	if _, err := sender.Write([]byte("first")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the flow to open", func() bool { return d.count() == 1 })

	// Past the idle window, then used again: the sweep that follows must find
	// it fresh.
	advance(2 * time.Minute)
	if _, err := sender.Write([]byte("second")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the second datagram", func() bool { return len(d.flow(0).sent()) == 2 })

	fwd.sweep()
	if d.flow(0).isClosed() {
		t.Fatal("a flow was closed while it was still carrying datagrams")
	}

	// A reply counts as use too: the container answering is the other direction
	// of the same exchange.
	advance(2 * time.Minute)
	d.flow(0).replies <- []byte("answer")
	buf := make([]byte, 64)
	if err := sender.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := sender.Read(buf); err != nil {
		t.Fatalf("the reply never arrived: %v", err)
	}

	fwd.sweep()
	if d.flow(0).isClosed() {
		t.Error("a flow was closed although a reply had just come back through it")
	}
}

// Close while a sweep is running is clean, which is only visible under -race.
func TestCloseDuringASweep(t *testing.T) {
	d := &dialer{}
	fwd, advance := forwardWithClock(t, d, time.Millisecond)

	senders := make([]net.Conn, 0, 8)
	for range 8 {
		s := senderTo(t, fwd)
		senders = append(senders, s)
		if _, err := s.Write([]byte("query")); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "every flow to open", func() bool { return d.count() == len(senders) })
	advance(time.Second)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 100 {
			fwd.sweep()
		}
	}()
	go func() {
		defer wg.Done()
		_ = fwd.Close()
	}()
	wg.Wait()

	if n := fwd.countFlows(); n != 0 {
		t.Errorf("the forward still holds %d flows after Close", n)
	}
}
