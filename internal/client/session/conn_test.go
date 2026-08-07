package session

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeConn counts how often a connection is established and closed.
type fakeConn struct{ closed atomic.Bool }

func (f *fakeConn) Close() error { f.closed.Store(true); return nil }

type fakeConnector struct {
	mu    sync.Mutex
	count int
	err   error
	conns []*fakeConn
}

func (f *fakeConnector) Connect(context.Context) (io.Closer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.count++
	c := &fakeConn{}
	f.conns = append(f.conns, c)
	return c, nil
}

func (f *fakeConnector) connections() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.count
}

type fakeInUse struct {
	volumes map[string]bool
	err     error
}

func (f *fakeInUse) VolumesInUse(context.Context) (map[string]bool, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.volumes, nil
}

func newLazy(connector *fakeConnector, inUse InUseReporter, idle time.Duration) *lazyConn {
	return &lazyConn{connect: connector, inUse: inUse, idle: idle}
}

// Nothing is connected until something asks. An idle workspace should cost a
// bound endpoint and nothing else.
func TestLazyConnDoesNotConnectUntilUsed(t *testing.T) {
	connector := &fakeConnector{}
	l := newLazy(connector, nil, time.Minute)

	if got := connector.connections(); got != 0 {
		t.Fatalf("connected %d times before any request", got)
	}

	_, release, err := l.acquire(t.Context())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	release()

	if got := connector.connections(); got != 1 {
		t.Errorf("connected %d times, want 1", got)
	}
}

// The second request reuses the first connection rather than opening another.
func TestLazyConnReusesTheConnection(t *testing.T) {
	connector := &fakeConnector{}
	l := newLazy(connector, nil, time.Minute)

	for range 5 {
		_, release, err := l.acquire(t.Context())
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		release()
	}
	if got := connector.connections(); got != 1 {
		t.Errorf("connected %d times, want 1", got)
	}
}

func TestLazyConnReleasesWhenIdle(t *testing.T) {
	connector := &fakeConnector{}
	l := newLazy(connector, &fakeInUse{}, time.Millisecond)

	_, release, err := l.acquire(t.Context())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	release()

	time.Sleep(5 * time.Millisecond)
	if !l.sweep(t.Context()) {
		t.Fatal("an idle connection with nothing using it was not released")
	}
	if !connector.conns[0].closed.Load() {
		t.Error("the connection was dropped but never closed")
	}

	// And it comes back on demand.
	_, release2, err := l.acquire(t.Context())
	if err != nil {
		t.Fatalf("re-acquire: %v", err)
	}
	release2()
	if got := connector.connections(); got != 2 {
		t.Errorf("connected %d times, want 2 (one reconnect)", got)
	}
}

// The constraint that makes this safe. A container holds its NFS mount for its
// whole life; dropping the tunnel underneath gives it EIO.
func TestLazyConnKeepsTheConnectionWhileVolumesAreInUse(t *testing.T) {
	connector := &fakeConnector{}
	inUse := &fakeInUse{volumes: map[string]bool{"rd-0011223344556677": true}}
	l := newLazy(connector, inUse, time.Millisecond)

	_, release, err := l.acquire(t.Context())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	release()
	time.Sleep(5 * time.Millisecond)

	if l.sweep(t.Context()) {
		t.Fatal("the connection was released while a container still held one of our volumes")
	}
	if connector.conns[0].closed.Load() {
		t.Error("the connection was closed while in use")
	}
}

// Somebody else's volumes are not a reason to stay connected.
func TestLazyConnIgnoresVolumesThatAreNotOurs(t *testing.T) {
	connector := &fakeConnector{}
	inUse := &fakeInUse{volumes: map[string]bool{"pgdata": true, "node_modules": true}}
	l := newLazy(connector, inUse, time.Millisecond)

	_, release, _ := l.acquire(t.Context())
	release()
	time.Sleep(5 * time.Millisecond)

	if !l.sweep(t.Context()) {
		t.Error("the connection was held open by volumes that are not ours")
	}
}

// A request in flight must not have the connection closed underneath it.
func TestLazyConnDoesNotReleaseWhileInUse(t *testing.T) {
	connector := &fakeConnector{}
	l := newLazy(connector, &fakeInUse{}, time.Millisecond)

	_, release, err := l.acquire(t.Context())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// Deliberately not released: a request is still running.
	time.Sleep(5 * time.Millisecond)

	if l.sweep(t.Context()) {
		t.Fatal("the connection was released with a request still in flight")
	}
	release()

	// Releasing restarts the idle window -- the connection was in use right up
	// until then -- so it is not immediately eligible.
	if l.sweep(t.Context()) {
		t.Error("the connection was released the instant the request finished, ignoring the idle period")
	}

	time.Sleep(5 * time.Millisecond)
	if !l.sweep(t.Context()) {
		t.Error("the connection was not released after the request finished and the idle period passed")
	}
}

// Unable to tell is not the same as safe to drop. Keeping a connection open
// costs a socket; dropping one still in use costs a container its filesystem.
func TestLazyConnKeepsTheConnectionWhenItCannotTell(t *testing.T) {
	connector := &fakeConnector{}
	inUse := &fakeInUse{err: errors.New("daemon unreachable")}
	l := newLazy(connector, inUse, time.Millisecond)

	_, release, _ := l.acquire(t.Context())
	release()
	time.Sleep(5 * time.Millisecond)

	if l.sweep(t.Context()) {
		t.Error("the connection was released without being able to check whether it was in use")
	}
}

func TestLazyConnIdleZeroNeverReleases(t *testing.T) {
	connector := &fakeConnector{}
	l := newLazy(connector, &fakeInUse{}, 0)

	_, release, _ := l.acquire(t.Context())
	release()
	time.Sleep(5 * time.Millisecond)

	if l.sweep(t.Context()) {
		t.Error("releasing is disabled but the connection was released")
	}
}

// A dead connection is replaced, and only if it is still the current one --
// closing a replacement that a concurrent caller already established would be
// worse than the failure being handled.
func TestLazyConnInvalidate(t *testing.T) {
	connector := &fakeConnector{}
	l := newLazy(connector, nil, time.Minute)

	conn, release, _ := l.acquire(t.Context())
	release()

	l.invalidate(conn)
	if _, release2, _ := l.acquire(t.Context()); true {
		release2()
	}
	if got := connector.connections(); got != 2 {
		t.Errorf("connected %d times, want 2 after invalidation", got)
	}

	// Invalidating a stale handle must not touch the current connection.
	current, release3, _ := l.acquire(t.Context())
	release3()
	l.invalidate(conn)
	if current.(*fakeConn).closed.Load() {
		t.Error("invalidating a stale connection closed the current one")
	}
}

func TestLazyConnPropagatesConnectFailures(t *testing.T) {
	connector := &fakeConnector{err: errors.New("no route to host")}
	l := newLazy(connector, nil, time.Minute)

	if _, _, err := l.acquire(t.Context()); err == nil {
		t.Error("a connection failure was not reported")
	}
}

// Concurrent requests share one connection rather than racing to open several.
func TestLazyConnIsSafeUnderConcurrency(t *testing.T) {
	connector := &fakeConnector{}
	l := newLazy(connector, &fakeInUse{}, time.Millisecond)

	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			_, release, err := l.acquire(t.Context())
			if err != nil {
				return
			}
			time.Sleep(time.Millisecond)
			release()
		})
	}
	for range 10 {
		wg.Go(func() { l.sweep(t.Context()) })
	}
	wg.Wait()

	if err := l.close(); err != nil {
		t.Errorf("close: %v", err)
	}
}
