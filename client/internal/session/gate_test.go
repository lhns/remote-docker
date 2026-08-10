package session

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeConn stands in for a workspace connection.
type fakeConn struct {
	id     int
	closed atomic.Bool
}

// gateFixture builds a gate over fake connections and records what it did.
type gateFixture struct {
	gate *connGate[*fakeConn]

	mu     sync.Mutex
	opened int
	conns  []*fakeConn

	busy    bool
	busyErr error
}

func newGate(t *testing.T, idle time.Duration) *gateFixture {
	t.Helper()
	f := &gateFixture{}
	f.gate = &connGate[*fakeConn]{
		open: func(context.Context) (*fakeConn, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.opened++
			c := &fakeConn{id: f.opened}
			f.conns = append(f.conns, c)
			return c, nil
		},
		shut: func(c *fakeConn) { c.closed.Store(true) },
		busy: func(context.Context, *fakeConn) (bool, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.busy, f.busyErr
		},
		idle: idle,
	}
	return f
}

func (f *gateFixture) openCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opened
}

func (f *gateFixture) setBusy(busy bool, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.busy, f.busyErr = busy, err
}

// Nothing connects until something asks. An idle workspace should cost a bound
// endpoint and nothing else.
func TestGateDoesNotConnectUntilUsed(t *testing.T) {
	f := newGate(t, time.Minute)

	if got := f.openCount(); got != 0 {
		t.Fatalf("opened %d connections before any request", got)
	}
	if _, ok := f.gate.current(); ok {
		t.Error("current() reports a connection before one was needed")
	}

	_, release, err := f.gate.acquire(t.Context())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	release()

	if got := f.openCount(); got != 1 {
		t.Errorf("opened %d connections, want 1", got)
	}
}

func TestGateReusesTheConnection(t *testing.T) {
	f := newGate(t, time.Minute)

	for range 5 {
		_, release, err := f.gate.acquire(t.Context())
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		release()
	}
	if got := f.openCount(); got != 1 {
		t.Errorf("opened %d connections, want 1", got)
	}
}

func TestGateReleasesWhenIdleAndReconnects(t *testing.T) {
	f := newGate(t, time.Millisecond)

	_, release, err := f.gate.acquire(t.Context())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	release()
	time.Sleep(5 * time.Millisecond)

	if !f.gate.sweep(t.Context()) {
		t.Fatal("an idle connection with nothing depending on it was not released")
	}
	if !f.conns[0].closed.Load() {
		t.Error("the connection was dropped but never shut")
	}
	if _, ok := f.gate.current(); ok {
		t.Error("current() still reports a connection after release")
	}

	_, release2, err := f.gate.acquire(t.Context())
	if err != nil {
		t.Fatalf("re-acquire: %v", err)
	}
	release2()
	if got := f.openCount(); got != 2 {
		t.Errorf("opened %d connections, want 2 (one reconnect)", got)
	}
}

// The constraint that makes this safe at all. A container holds its NFS mount
// for its whole life; dropping the tunnel underneath gives it EIO.
func TestGateKeepsTheConnectionWhileSomethingDependsOnIt(t *testing.T) {
	f := newGate(t, time.Millisecond)
	f.setBusy(true, nil)

	_, release, _ := f.gate.acquire(t.Context())
	release()
	time.Sleep(5 * time.Millisecond)

	if f.gate.sweep(t.Context()) {
		t.Fatal("the connection was released while something still depended on it")
	}
	if f.conns[0].closed.Load() {
		t.Error("the connection was shut while in use")
	}

	// And once nothing does, it goes.
	f.setBusy(false, nil)
	if !f.gate.sweep(t.Context()) {
		t.Error("the connection was not released after its dependents went away")
	}
}

// Unable to tell is not the same as safe to drop. Holding a connection costs a
// socket; dropping one still in use costs a container its filesystem.
func TestGateKeepsTheConnectionWhenItCannotTell(t *testing.T) {
	f := newGate(t, time.Millisecond)
	f.setBusy(false, errors.New("daemon unreachable"))

	_, release, _ := f.gate.acquire(t.Context())
	release()
	time.Sleep(5 * time.Millisecond)

	if f.gate.sweep(t.Context()) {
		t.Error("the connection was released without being able to check whether it was in use")
	}
}

// A request in flight must not have the connection shut underneath it.
func TestGateDoesNotReleaseWhileInUse(t *testing.T) {
	f := newGate(t, time.Millisecond)

	_, release, err := f.gate.acquire(t.Context())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	time.Sleep(5 * time.Millisecond)

	if f.gate.sweep(t.Context()) {
		t.Fatal("the connection was released with a request still in flight")
	}
	release()

	// Releasing restarts the idle window -- it was in use right up until now.
	if f.gate.sweep(t.Context()) {
		t.Error("the connection was released the instant the request finished, ignoring the idle period")
	}
	time.Sleep(5 * time.Millisecond)
	if !f.gate.sweep(t.Context()) {
		t.Error("the connection was not released once idle again")
	}
}

func TestGateIdleZeroNeverReleases(t *testing.T) {
	f := newGate(t, 0)

	_, release, _ := f.gate.acquire(t.Context())
	release()
	time.Sleep(5 * time.Millisecond)

	if f.gate.sweep(t.Context()) {
		t.Error("releasing is disabled but the connection was released")
	}
}

func TestGatePropagatesOpenFailures(t *testing.T) {
	f := newGate(t, time.Minute)
	f.gate.open = func(context.Context) (*fakeConn, error) {
		return nil, errors.New("no route to host")
	}

	if _, _, err := f.gate.acquire(t.Context()); err == nil {
		t.Error("a connection failure was not reported")
	}
	// And a failure must not leave the gate believing it holds one.
	if _, ok := f.gate.current(); ok {
		t.Error("a failed connection was recorded as held")
	}
}

func TestGateClose(t *testing.T) {
	f := newGate(t, time.Minute)

	_, release, _ := f.gate.acquire(t.Context())
	release()

	f.gate.close()
	if !f.conns[0].closed.Load() {
		t.Error("close left the connection open")
	}
	// Twice is safe: Session.Close and a sweep can race.
	f.gate.close()
}

// Concurrent requests share one connection rather than racing to open several,
// and a sweep running alongside them must not corrupt the count.
func TestGateIsSafeUnderConcurrency(t *testing.T) {
	f := newGate(t, time.Millisecond)

	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			conn, release, err := f.gate.acquire(t.Context())
			if err != nil {
				return
			}
			if conn.closed.Load() {
				t.Error("acquired a connection that had already been shut")
			}
			time.Sleep(time.Millisecond)
			release()
		})
	}
	for range 20 {
		wg.Go(func() { f.gate.sweep(t.Context()) })
	}
	wg.Wait()
	f.gate.close()
}
