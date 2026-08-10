package session

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/lhns/remote-docker/internal/logx"
)

// connGate holds a connection open only while it is needed.
//
// An idle workspace otherwise costs a live SSH connection, a keepalive every
// fifteen seconds, an events stream and a reconcile ticker -- per workspace,
// and the whole point of contexts is to have several. The endpoint itself is
// cheap and stays bound, because it is how we are found; only what sits behind
// it comes and goes.
//
// The hard constraint is that a connection cannot be dropped while anything
// still depends on it. For this project that means a container holding an NFS
// mount from us: the remote daemon mounts the volume at container start and
// keeps it for the container's life, so pulling the tunnel out gives that
// container EIO. `soft,timeo=30` makes the failure clean rather than a hang,
// but it is still a failure -- idleness alone is never enough.
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

	// idle is how long a connection may sit unused before being considered
	// for release. Zero or negative disables releasing.
	idle time.Duration

	log *slog.Logger

	mu       sync.Mutex
	conn     T
	held     bool
	lastUsed time.Time
	users    int
}

// acquire returns a live connection, establishing one if needed, and a
// function to call when finished with it.
//
// The user count is what stops a sweep closing a connection out from under a
// request in flight.
func (g *connGate[T]) acquire(ctx context.Context) (T, func(), error) {
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
