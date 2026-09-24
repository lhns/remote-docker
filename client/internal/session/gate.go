package session

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/lhns/remote-docker/core/logx"
)

// connGate holds a connection open only while it is needed (ADR 0015). It is
// never dropped while busy says something depends on it: a container holding
// an NFS mount through it would get EIO. Generic so the policy is testable
// without SSH.
type connGate[T any] struct {
	open func(ctx context.Context) (T, error)
	shut func(T)

	// busy reports whether anything depends on the connection. An error means
	// cannot tell, which counts as busy.
	busy func(ctx context.Context, conn T) (bool, error)

	// alive reports whether a held connection still works; nil means always.
	// It must not do I/O: it runs before every request.
	alive func(conn T) bool

	// idle is how long an unused connection is kept; <= 0 never releases.
	idle time.Duration

	log *slog.Logger

	mu       sync.Mutex
	conn     T
	held     bool
	lastUsed time.Time
	// users counts leases, not leases on the current connection: a stream over
	// a connection since dropped still releases one, so invalidate must not
	// zero it or the count goes negative and a sweep closes one in use.
	users  int
	drops  int
	lastDr time.Time
}

// invalidate drops a held connection that has died, returning it to be shut.
// Without it the dead connection is handed to every later request, and sweep
// asking it busy gets an error and keeps it forever.
func (g *connGate[T]) invalidate() (T, bool) {
	var zero T

	g.mu.Lock()
	defer g.mu.Unlock()

	if !g.held || g.alive == nil || g.alive(g.conn) {
		return zero, false
	}
	dead := g.conn
	g.held = false
	g.conn = zero
	g.drops++
	g.lastDr = time.Now()
	return dead, true
}

// dropped reports how many times the connection was found dead, and when last.
func (g *connGate[T]) dropped() (int, time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.drops, g.lastDr
}

// acquire returns a live connection, opening one if needed, and a release func.
func (g *connGate[T]) acquire(ctx context.Context) (T, func(), error) {
	// Shut outside the lock: it waits on the goroutines riding the connection.
	if dead, ok := g.invalidate(); ok {
		g.logger().Warn("the connection to the workspace had dropped; opening another")
		g.shut(dead)
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if !g.held {
		conn, err := g.open(ctx)
		if err != nil {
			var zero T
			return zero, nil, err
		}
		g.conn = conn
		g.held = true
	}

	g.users++
	g.lastUsed = time.Now()
	conn := g.conn

	var once sync.Once
	return conn, func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.users--
			g.lastUsed = time.Now()
		})
	}, nil
}

// sweep releases the connection if it is dead, or idle with nothing depending
// on it, and reports whether it did.
func (g *connGate[T]) sweep(ctx context.Context) bool {
	// Dropped rather than asked: busy on a dead connection errors, which keeps.
	if dead, ok := g.invalidate(); ok {
		g.shut(dead)
		return true
	}

	g.mu.Lock()
	if !g.held || g.idle <= 0 || g.users > 0 || time.Since(g.lastUsed) < g.idle {
		g.mu.Unlock()
		return false
	}
	conn := g.conn
	g.mu.Unlock()

	busy, err := g.busy(ctx, conn)
	if err != nil {
		g.logger().Warn("keeping the connection", "err", err)
		return false
	}
	if busy {
		return false
	}

	g.mu.Lock()
	// A request may have arrived while busy was asked.
	if !g.held || g.users > 0 || time.Since(g.lastUsed) < g.idle {
		g.mu.Unlock()
		return false
	}
	g.held = false
	var zero T
	g.conn = zero
	g.mu.Unlock()

	g.shut(conn)
	g.logger().Info("released the idle connection; it reopens on the next request")
	return true
}

// lastUse reports when the connection was last used, and whether it is in use
// now. The zero time means never, and such a session should be reclaimed first.
func (g *connGate[T]) lastUse() (time.Time, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lastUsed, g.users > 0
}

// currentLive returns the connection only while it still works. Holding a
// dead one once made `remote status` print "ready" while mounts returned EIO.
func (g *connGate[T]) currentLive() (T, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if !g.held || (g.alive != nil && !g.alive(g.conn)) {
		var zero T
		return zero, false
	}
	return g.conn, true
}

// close tears the connection down for good.
func (g *connGate[T]) close() {
	g.mu.Lock()
	if !g.held {
		g.mu.Unlock()
		return
	}
	conn := g.conn
	g.held = false
	var zero T
	g.conn = zero
	g.mu.Unlock()

	g.shut(conn)
}

func (g *connGate[T]) logger() *slog.Logger {
	return logx.Or(g.log)
}
