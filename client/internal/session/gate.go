package session

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/lhns/remote-docker/core/logx"
)

// connGate holds a connection open only while it is needed.
//
// An idle workspace otherwise costs a live SSH connection, a keepalive every
// fifteen seconds, an events stream and a reconcile ticker, per workspace,
// and the whole point of contexts is to have several. The endpoint itself is
// cheap and stays bound, because it is how we are found; only what sits behind
// it comes and goes.
//
// The hard constraint is that a connection cannot be dropped while anything
// still depends on it. For this project that means a container holding an NFS
// mount from us: the remote daemon mounts the volume at container start and
// keeps it for the container's life, so pulling the tunnel out gives that
// container EIO. `soft,timeo=30` makes the failure clean rather than a hang,
// but it is still a failure. Idleness alone is never enough.
//
// Generic over the connection type so the policy can be tested without an SSH
// client, a daemon, or a workspace.
type connGate[T any] struct {
	// open establishes a connection.
	open func(ctx context.Context) (T, error)

	// shut tears one down.
	shut func(T)

	// busy reports whether anything still depends on the connection. An error
	// means "cannot tell", which is treated as busy.
	busy func(ctx context.Context, conn T) (bool, error)

	// alive reports whether a held connection can still carry anything. Nil
	// means always, which is what a gate over something with no transport
	// wants.
	//
	// UNLIKE busy this must not do I/O: it is consulted on the acquire path,
	// under no lock and before every request. busy asks the workspace a
	// question; this asks the connection whether it is still there.
	alive func(conn T) bool

	// idle is how long a connection may sit unused before being considered
	// for release. Zero or negative disables releasing.
	idle time.Duration

	log *slog.Logger

	mu       sync.Mutex
	conn     T
	held     bool
	lastUsed time.Time
	// users counts LEASES, not leases on the connection currently held. A
	// stream opened over a connection that has since died still holds one and
	// still releases it when it closes, so invalidate leaves this alone:
	// zeroing it would drive the count negative on that release and let a sweep
	// close a connection genuinely in use.
	users  int
	drops  int
	lastDr time.Time
}

// invalidate drops a connection that has died, returning it to be shut.
//
// Nothing else clears held. Without this a connection that died between two
// requests is handed to every request after it, and the sweep asks the dead
// connection whether anything depends on it, gets an error, and the "cannot
// tell means keep" rule below makes it permanently unreleasable. That was the
// wedge that needed `remote restart` to clear.
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

// drops reports how many times this gate has found its connection dead, and
// when it last did.
func (g *connGate[T]) dropped() (int, time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.drops, g.lastDr
}

// acquire returns a live connection, establishing one if needed, and a
// function to call when finished with it.
//
// The user count is what stops a sweep closing a connection out from under a
// request in flight.
func (g *connGate[T]) acquire(ctx context.Context) (T, func(), error) {
	// Shut outside the lock: tearing a connection down waits on the goroutines
	// riding it, and holding the gate across that would stall every request.
	// Only one caller is ever handed the dead connection back, so however many
	// arrive together it is shut once.
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
			// Releasing restarts the idle window: the connection was in use
			// right up until now.
			g.lastUsed = time.Now()
		})
	}, nil
}

// sweep releases the connection if it has been idle and nothing depends on it.
// It reports whether the connection was released.
func (g *connGate[T]) sweep(ctx context.Context) bool {
	// A dead connection is dropped rather than asked. Asking is what wedged it:
	// busy fails over a dead transport, and "cannot tell means keep" then holds
	// it forever.
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

	// Asked outside the lock: it is a round trip to the workspace, and holding
	// the lock across it would stall every request.
	busy, err := g.busy(ctx, conn)
	if err != nil {
		// Unable to tell is not the same as safe to drop. Holding a connection
		// costs a socket; dropping one still in use costs a running container
		// its filesystem.
		g.logger().Warn("keeping the connection", "err", err)
		return false
	}
	if busy {
		return false
	}

	g.mu.Lock()
	// Re-check under the lock: a request may have arrived while we asked.
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

// lastUse reports when the connection was last used, and whether anything is
// using it right now.
//
// The zero time means never, which the caller must handle rather than read as
// "just now": a session that has served nothing has no last use, and it is the
// one that should be reclaimed soonest.
//
// Separate from sweep because the daemon's lifetime asks a different question
// from the connection's: sweep decides whether to drop a connection that can
// be reopened, this decides whether to end a process that cannot.
func (g *connGate[T]) lastUse() (time.Time, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lastUsed, g.users > 0
}

// current returns the connection if one is held.
func (g *connGate[T]) current() (T, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.conn, g.held
}

// currentLive returns the connection only while it can still carry something.
//
// Holding a connection object is not the same as having a connection, and
// reporting the first as the second is how `remote status` printed "ready"
// while every container's filesystem returned EIO.
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

// logger is the gate's, or silence. A nil *slog.Logger panics on use rather
// than doing nothing, and a gate built by a test has none.
func (g *connGate[T]) logger() *slog.Logger {
	if g.log == nil {
		return logx.Discard()
	}
	return g.log
}
