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
	dead   atomic.Bool
}

// gateFixture builds a gate over fake connections and records what it did.
type gateFixture struct {
	gate *connGate[*fakeConn]

	mu     sync.Mutex
	opened int
	conns  []*fakeConn

	busy      bool
	busyErr   error
	busyCalls int
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
			f.busyCalls++
			return f.busy, f.busyErr
		},
		alive: func(c *fakeConn) bool { return !c.dead.Load() },
		idle:  idle,
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

	// Releasing restarts the idle window: it was in use right up until now.
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

// A connection that died is replaced, not handed out again.
//
// Detecting the death is not enough: unless `held` is cleared, every request
// after a drop is handed the same dead connection and fails in a way that
// reads as the workspace refusing rather than as a tunnel that went away.
func TestGateReplacesADeadConnection(t *testing.T) {
	f := newGate(t, time.Minute)
	ctx := context.Background()

	conn, release, err := f.gate.acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	release()
	conn.dead.Store(true)

	again, release2, err := f.gate.acquire(ctx)
	if err != nil {
		t.Fatalf("acquire after the drop: %v", err)
	}
	defer release2()

	if again == conn {
		t.Fatal("the dead connection was handed out again")
	}
	if !conn.closed.Load() {
		t.Error("the dead connection was dropped without being shut")
	}
	if n := f.openCount(); n != 2 {
		t.Errorf("opened %d connections, want 2", n)
	}
	if drops, _ := f.gate.dropped(); drops != 1 {
		t.Errorf("counted %d drops, want 1", drops)
	}
}

// The wedge itself: a dead connection must be DROPPED rather than asked whether
// anything depends on it.
//
// busy fails over a dead transport, and "cannot tell means keep" then held the
// connection forever, so the session could never reopen and `remote restart`
// was the only way out.
func TestGateDropsADeadConnectionWithoutAskingIt(t *testing.T) {
	f := newGate(t, time.Millisecond)
	ctx := context.Background()

	conn, release, err := f.gate.acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	release()

	conn.dead.Store(true)
	f.setBusy(false, errors.New("connection is dead"))
	time.Sleep(2 * time.Millisecond)

	if !f.gate.sweep(ctx) {
		t.Fatal("the sweep kept a dead connection")
	}
	f.mu.Lock()
	calls := f.busyCalls
	f.mu.Unlock()
	if calls != 0 {
		t.Errorf("the dead connection was asked whether it was busy %d times", calls)
	}
}

// Whatever arrives together, the dead connection is shut once.
func TestGateShutsADeadConnectionOnce(t *testing.T) {
	f := newGate(t, time.Minute)
	ctx := context.Background()

	conn, release, err := f.gate.acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	release()

	var shuts atomic.Int32
	f.gate.shut = func(c *fakeConn) {
		c.closed.Store(true)
		shuts.Add(1)
	}
	conn.dead.Store(true)

	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			c, rel, err := f.gate.acquire(ctx)
			if err != nil {
				t.Error(err)
				return
			}
			if c.dead.Load() {
				t.Error("a dead connection was handed out")
			}
			rel()
		})
	}
	wg.Wait()

	if n := shuts.Load(); n != 1 {
		t.Errorf("the dead connection was shut %d times, want 1", n)
	}
}

// A lease outstanding over a connection that then dies must still release
// cleanly, and must not make the gate think a live connection is in use by
// somebody who has gone.
//
// The lease count is over LEASES, not over the connection currently held, so a
// stream opened before the drop still decrements when it closes. Zeroing the
// count on invalidate would drive it negative here.
func TestGateDeadConnectionWithALeaseOutstanding(t *testing.T) {
	f := newGate(t, time.Millisecond)
	ctx := context.Background()

	conn, releaseOld, err := f.gate.acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	conn.dead.Store(true)

	fresh, releaseNew, err := f.gate.acquire(ctx)
	if err != nil {
		t.Fatalf("acquire after the drop: %v", err)
	}
	if fresh == conn {
		t.Fatal("the dead connection was handed out again")
	}
	releaseNew()

	// The stream over the dead connection notices and closes.
	releaseOld()

	time.Sleep(2 * time.Millisecond)
	if !f.gate.sweep(ctx) {
		t.Error("the fresh connection could not be released once idle")
	}
}

// Aliveness is optional: a gate over something with no transport must behave
// exactly as it did.
func TestGateWithoutALivenessCheck(t *testing.T) {
	f := newGate(t, time.Minute)
	f.gate.alive = nil
	ctx := context.Background()

	conn, release, err := f.gate.acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	release()
	conn.dead.Store(true)

	again, release2, err := f.gate.acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer release2()
	if again != conn {
		t.Error("a gate with no liveness check replaced its connection")
	}
}

// currentLive answers what Status asks: not "do we hold one" but "have we got
// one".
func TestCurrentLive(t *testing.T) {
	f := newGate(t, time.Minute)

	if _, ok := f.gate.currentLive(); ok {
		t.Error("a gate holding nothing reported a live connection")
	}

	conn, release, err := f.gate.acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	release()
	if _, ok := f.gate.currentLive(); !ok {
		t.Error("a live connection was reported dead")
	}

	conn.dead.Store(true)
	if _, ok := f.gate.currentLive(); ok {
		t.Error("a dead connection was reported live")
	}
	if _, ok := f.gate.current(); !ok {
		t.Error("current stopped reporting what is held")
	}
}
