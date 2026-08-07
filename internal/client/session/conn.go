package session

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/lhns/remote-docker/pkg/workspace"
)

// InUseReporter says whether anything on the workspace still depends on this
// client's connection -- specifically, whether any container holds a volume
// backed by our NFS server.
type InUseReporter interface {
	VolumesInUse(ctx context.Context) (map[string]bool, error)
}

// Connector establishes the underlying connection.
type Connector interface {
	Connect(ctx context.Context) (io.Closer, error)
}

// lazyConn holds the workspace connection open only while it is needed.
//
// An idle workspace otherwise costs a live SSH connection, a keepalive every
// fifteen seconds, an events stream and a reconcile ticker -- per workspace,
// and the whole point of contexts is to have several. The endpoint itself is
// cheap and stays bound, because it is how we are found.
//
// The hard constraint is that the connection cannot be dropped while any
// container holds an NFS mount from us. The remote daemon mounts the volume at
// container start and keeps it for the container's life; pulling the tunnel
// out from under it gives that container EIO. `soft,timeo=30` makes the
// failure clean rather than a hang, but it is still a failure -- so idleness
// alone is never enough to disconnect.
type lazyConn struct {
	connect Connector
	inUse   InUseReporter

	// idle is how long the connection may sit unused before being considered
	// for release. Zero disables releasing entirely.
	idle time.Duration

	log Logger

	mu       sync.Mutex
	conn     io.Closer
	lastUsed time.Time
	users    int
}

// acquire returns a live connection, establishing one if needed, and a release
// function the caller must invoke when done with it.
//
// The counter matters: a request in flight must not have the connection closed
// underneath it by the idle sweep.
func (l *lazyConn) acquire(ctx context.Context) (io.Closer, func(), error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.conn == nil {
		conn, err := l.connect.Connect(ctx)
		if err != nil {
			return nil, nil, err
		}
		l.logf("connected to the workspace")
		l.conn = conn
	}

	l.users++
	l.lastUsed = time.Now()
	conn := l.conn

	var once sync.Once
	release := func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.users--
			l.lastUsed = time.Now()
		})
	}
	return conn, release, nil
}

// invalidate drops the current connection so the next acquire establishes a
// new one. Used when a connection turns out to be dead.
func (l *lazyConn) invalidate(conn io.Closer) {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Only if it is still the current one: a concurrent caller may already
	// have replaced it, and closing the replacement would be worse than the
	// failure being handled.
	if l.conn != conn {
		return
	}
	_ = l.conn.Close()
	l.conn = nil
}

// sweep releases the connection if it has been idle and nothing depends on it.
// It reports whether the connection was released.
func (l *lazyConn) sweep(ctx context.Context) bool {
	l.mu.Lock()
	if l.conn == nil || l.idle == 0 || l.users > 0 || time.Since(l.lastUsed) < l.idle {
		l.mu.Unlock()
		return false
	}
	conn := l.conn
	l.mu.Unlock()

	// Checked outside the lock, because it is a round trip to the workspace
	// and holding the lock across it would stall every request.
	busy, err := l.volumesInUse(ctx)
	if err != nil {
		// Unable to tell. Keeping a connection open costs a socket; dropping
		// one that is still in use costs a running container its filesystem.
		l.logf("not releasing the connection: %v", err)
		return false
	}
	if busy {
		return false
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	// Re-check under the lock: a request may have arrived while we asked.
	if l.conn != conn || l.users > 0 || time.Since(l.lastUsed) < l.idle {
		return false
	}
	_ = l.conn.Close()
	l.conn = nil
	l.logf("released the idle connection; it will reopen on the next request")
	return true
}

// volumesInUse reports whether any container holds one of our share volumes.
func (l *lazyConn) volumesInUse(ctx context.Context) (bool, error) {
	if l.inUse == nil {
		return false, nil
	}
	inUse, err := l.inUse.VolumesInUse(ctx)
	if err != nil {
		return false, err
	}
	for name := range inUse {
		if isManagedVolume(name) {
			return true, nil
		}
	}
	return false, nil
}

// close tears the connection down for good.
func (l *lazyConn) close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn == nil {
		return nil
	}
	err := l.conn.Close()
	l.conn = nil
	return err
}

func (l *lazyConn) logf(format string, args ...any) {
	if l.log != nil {
		l.log.Printf(format, args...)
	}
}

// isManagedVolume reports whether a volume is one of ours, by prefix.
//
// The prefix alone is enough here, unlike garbage collection where it is not:
// a false positive keeps a connection open a while longer, where a false
// positive there would delete a user's data.
func isManagedVolume(name string) bool {
	return workspace.IsManagedVolume(name)
}
