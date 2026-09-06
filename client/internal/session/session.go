// Package session wires the client's parts into one live connection to a
// workspace: the SSH transport, the NFS export behind a reverse forward, the
// Docker API proxy, and the port forwards.
//
// The endpoint is bound for the life of the session, because it is how the
// Docker client finds us. The connection behind it is established on first use
// and released when nothing needs it, so an idle workspace costs a socket and
// nothing more.
package session

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lhns/remote-docker/client/internal/config"
	"github.com/lhns/remote-docker/client/internal/ports"
	"github.com/lhns/remote-docker/client/internal/proxy"
	"github.com/lhns/remote-docker/client/internal/rewrite"
	"github.com/lhns/remote-docker/core-client/fswatch"
	"github.com/lhns/remote-docker/core-client/nfsserve"
	"github.com/lhns/remote-docker/core-client/tunnelclient"
	"github.com/lhns/remote-docker/core/logx"
	"github.com/lhns/remote-docker/core/tunnel"
	"github.com/lhns/remote-docker/core/workspace"
	"github.com/lhns/remote-docker/dircache"
)

// Options configure a session.
type Options struct {
	Config config.Config

	// WorkDir is the directory exported at /cwd.
	WorkDir string

	// Endpoint overrides where the Docker API is served locally.
	Endpoint string

	// IdleTimeout is how long the workspace connection may sit unused before
	// being released. Zero uses DefaultIdleTimeout; negative never releases.
	IdleTimeout time.Duration

	// Role says what this session is for, and decides every behaviour that
	// differs between the two. See the constants: it replaces what would
	// otherwise be several independent switches that must agree.
	Role Role

	// Watch replays this machine's filesystem changes into the workspace, so
	// watchers in containers notice them (ADR 0016). Off by default: nothing
	// is watched and no channel is opened.
	Watch        fswatch.Mode
	WatchBudget  int
	WatchExclude []string

	// Mode is what a share gets on each axis the mount left unset, and
	// ModePaths overrides it per directory (ADR 0042). Parsed by the command
	// layer, which is where a bad value is reported.
	Mode      workspace.Mode
	ModePaths map[string]workspace.Mode

	// PosixSource reports the POSIX path a shell may have rewritten a bind
	// source into. Supplied by the command layer, which is where the shell is
	// known; nil everywhere else, which is every platform but Windows.
	PosixSource func(source string) string

	// Version is the build this session is running, reported to anything
	// asking whether it matches the client talking to it.
	Version string

	Log *slog.Logger
}

// Role is what a session is for. There are two, and the difference between
// them is three refusals that always apply together.
type Role int

const (
	// Query only asks the workspace things, as `status` and `gc` do. It binds
	// nothing and exports nothing, and each half of that is load-bearing:
	//
	//   - It must not bind the local Docker endpoint. These commands never use
	//     it, and on Windows the named-pipe bind genuinely excludes, so a
	//     `status` that bound it could not run while a session was running --
	//     precisely when somebody runs `status`.
	//
	//   - It must not export files. An account has exactly one reverse-tunnel
	//     port (ADR 0003), so a command that does not need the export must not
	//     take it, or it fails whenever a real session holds it.
	//
	// It is also silent. A command's output belongs to the command: `status`
	// prints a table and `remote-docker docker run` prints a container's
	// stdout, and progress chatter interleaved with either is noise in the
	// success case. Problems are still reported, always to stderr.
	Query Role = iota

	// Host serves the workspace to this machine: it binds the endpoint,
	// exports the working directory, forwards published ports, and reports
	// what it is doing. This is what `start` runs, in the foreground or
	// behind one.
	//
	// Exactly one of these can exist per account, and it fails rather than
	// half-working when the export port is already taken, and two of them is a
	// genuine conflict and saying so beats a session that silently serves no
	// files.
	Host
)

// hosting is the single question the rest of this package asks. Deliberately
// not a serves()/exports()/narrates() trio: those are three names for one bit,
// and three names can be given three different answers.
func (r Role) hosting() bool { return r == Host }

func (r Role) String() string {
	if r == Host {
		return "host"
	}
	return "query"
}

// DefaultIdleTimeout balances a reconnect against holding a connection nobody
// is using: long enough that someone working normally never notices, short
// enough that a workspace left open overnight is not holding anything.
const DefaultIdleTimeout = time.Minute

// Session serves the local Docker endpoint for one workspace.
type Session struct {
	Endpoint string

	opts     Options
	listener net.Listener
	proxy    *proxy.Proxy

	// registry outlives any single connection: share ids are derived from the
	// path, so a reconnect reuses the same exports and the same remote volumes
	// rather than orphaning a set per connection.
	registry *nfsserve.Registry

	// nfs outlives a connection for a harder reason than the registry does.
	// NFSv3 handles are opaque, the kernel keeps presenting the ones it was
	// given, and they live in this server's handle cache -- so a server built
	// per connection hands out a fresh set on every reconnect and every
	// container that was already running reads "Stale file handle" forever.
	// Nothing announces it: the mount is there, the port answers, the files
	// are gone. Measured in test/nfs-resilience.sh, section 10.
	nfs *nfsserve.Server

	// clientID names this MACHINE, derived from its key on the first connect.
	// Empty before then, which nothing that uses it can observe: everything
	// asking is downstream of a connection.
	clientID string

	// shares is what this workspace has been asked to export, across sessions.
	// Nil on a session that does not serve, which is how a query session comes
	// to restore nothing.
	shares *shareStore

	// cache fills, invalidates and writes back the caches of delegated shares
	// (ADR 0044). The engine is dircache and knows nothing of this session;
	// what is wired into it below is where the files are and how to reach a
	// workspace. Nil on a query session, which caches nothing.
	cache *dircache.Cache

	// rtt is the tunnel's round trip as last measured, in nanoseconds, for
	// the prefetch policy. Zero until a connection has been made.
	rtt atomic.Int64

	// watch outlives any single connection too, and for the same reason the
	// registry does: watches are a local resource, and re-walking a large
	// tree on every idle reconnect would cost more than the connection. Only
	// the sink comes and goes.
	watch      *fswatch.Watcher
	started    time.Time
	notifyOnce sync.Once

	gate *connGate[*liveConn]

	stopped  chan struct{}
	stopOnce sync.Once

	// dormant is set once Standby has released the workspace, and cleared by
	// the next request. The endpoint is served either way.
	dormantMu sync.Mutex
	dormant   bool

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	once   sync.Once
}

// liveConn is everything that exists only while connected.
type liveConn struct {
	ssh       *tunnelclient.Client
	info      workspace.Info
	api       *proxy.APIClient
	rewriter  *rewrite.Rewriter
	guard     *rewrite.Guard
	nfsTunnel net.Listener
	ports     *ports.Manager

	// clockSkew is the workspace's clock minus this machine's, measured when
	// this connection was made and used for one comparison: which side wrote
	// last when a file changed in both places (ADR 0044).
	clockSkew time.Duration

	// notify is the change-notification channel, nil when the workspace does
	// not support it or watching is off.
	notify io.Closer

	// cacheChan serves delegated shares (ADR 0044). Opened on first use, and
	// once: a session that mounts no delegated share never opens it, and a
	// workspace too old to serve it must not be asked twice per container.
	cacheOnce sync.Once
	cacheChan *cacheChannel

	// machine holds a local machine open, nil for a workspace that is simply
	// there. Closing it lets the machine shut itself down.
	machine io.Closer

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// Open binds the local Docker endpoint. It does not connect to the workspace;
// that happens on the first request.
func Open(ctx context.Context, opts Options) (*Session, error) {
	if err := opts.Config.RequireHost(); err != nil {
		return nil, err
	}
	if opts.IdleTimeout == 0 {
		opts.IdleTimeout = DefaultIdleTimeout
	}

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))

	// go-nfs logs to stderr through a package-level logger of its own. Point
	// it at ours before anything can serve, or it writes past the client's
	// logging and straight onto the user's terminal.
	nfsserve.SetLogger(opts.Log)

	s := &Session{
		opts:    opts,
		ctx:     runCtx,
		cancel:  cancel,
		stopped: make(chan struct{}),
		// Corrected once the workspace reports its uid. Nothing is served
		// before that, so the defaults are never observed.
		registry: nfsserve.NewRegistry(defaultAttrs()),
	}

	// One list for the cache and the watcher, resolved once. The cache walks
	// what the watcher invalidates, so a directory the watcher does not see is
	// one the cache would fill and then serve stale for good.
	exclude := fswatch.ExcludesOr(opts.WatchExclude)

	// Only a session that serves may restore a share. A query session exports
	// nothing, and giving it the record would let asking a question re-export
	// a directory.
	if opts.Role.hosting() {
		s.shares = newShareStore(config.SharesPath(opts.Config.Name), opts.Log)
		s.registry.Restore = s.shares.restore
		policy, err := dircache.ParsePolicy(opts.Config.Prefetch)
		if err != nil {
			return nil, fmt.Errorf("prefetch: %w", err)
		}
		s.cache = &dircache.Cache{
			Store:   s.liveStore,
			Record:  newCachedStore(config.CachedPath(opts.Config.Name), opts.Log),
			Exclude: exclude,
			Budget:  dircache.Budget{Files: opts.Config.CacheFiles, Bytes: opts.Config.CacheBytes},
			Skew:    s.skew,
			Log:     opts.Log,
			Ctx:     runCtx,
			Policy:  policy,
			Link:    s.link,
		}
		// Set before the first share is registered: a share's filesystem is
		// built with it.
		s.registry.OnRead = s.cache.Touch
		s.nfs = nfsserve.New(s.registry, opts.Log)
	}

	if _, err := s.registry.RegisterCWD(opts.WorkDir); err != nil {
		cancel()
		return nil, err
	}

	if opts.Watch != fswatch.ModeOff && opts.Role.hosting() {
		watcher, err := fswatch.New(fswatch.Options{
			Mode:    opts.Watch,
			Budget:  opts.WatchBudget,
			Exclude: exclude,
			Log:     opts.Log,
		})
		if err != nil {
			cancel()
			return nil, err
		}
		s.watch = watcher
		// Every change, before the mode decides what a container's watcher can
		// be shown: a deletion cannot be replayed faithfully over NFS, which
		// is what ModePartial is about, but it can be applied to a cache
		// exactly, and a cached copy of a file that is gone is the one way
		// this mode can be wrong rather than slow (ADR 0044).
		s.watch.SetObserver(cacheObserver{cache: s.cache})
		s.syncWatch()
		s.wg.Go(func() { s.reconcileShares(runCtx, shareReconcileInterval) })
	}

	s.gate = &connGate[*liveConn]{
		open: s.connect,
		shut: func(live *liveConn) {
			// The watches stay; only the channel to the agent goes. What was
			// missed meanwhile is announced on the next connection rather
			// than quietly forgotten.
			if s.watch != nil {
				s.watch.ClearSink()
			}
			live.close()
		},
		busy: s.hasLiveDependents,
		// Asked before every request, so a dropped connection is replaced
		// rather than handed out again.
		alive: func(live *liveConn) bool { return live.ssh.Alive() },
		idle:  opts.IdleTimeout,
		log:   opts.Log,
	}

	s.started = time.Now()
	s.proxy = &proxy.Proxy{Dialer: s, Rewriter: s, Log: opts.Log}

	if opts.Role.hosting() {
		// Only a session that serves the endpoint answers for it. One that does
		// not would be claiming to be a daemon it is not.
		s.proxy.Control = s

		if err := s.listen(opts.Endpoint); err != nil {
			cancel()
			// The watcher is already walking the working directory by now, and
			// it holds handles the context does not: cancelling alone leaves
			// it running in a process that is about to report a failure.
			if s.watch != nil {
				_ = s.watch.Close()
			}
			return nil, err
		}
	} else {
		// Still reported, because commands print it and a session that serves
		// nothing should still be able to say where the endpoint would be.
		s.Endpoint = proxy.DockerHost(opts.Endpoint)
	}

	if opts.IdleTimeout > 0 {
		s.wg.Go(s.sweepIdle)
	}
	return s, nil
}

func (s *Session) listen(endpoint string) error {
	l, err := proxy.Listen(endpoint)
	if err != nil {
		return err
	}
	s.listener = l
	s.Endpoint = proxy.DockerHost(endpoint)

	s.wg.Go(func() {
		if err := s.proxy.Serve(s.ctx, l); err != nil {
			s.log().Warn("the docker endpoint stopped", "err", err)
		}
	})
	return nil
}

// DialDocker satisfies proxy.Dialer. Every request arrives here, which makes
// "connect on first use" a single place rather than a policy scattered across
// the session.
func (s *Session) DialDocker(ctx context.Context) (io.ReadWriteCloser, error) {
	live, done, err := s.acquire(ctx)
	if err != nil {
		return nil, err
	}
	stream, err := live.ssh.OpenStream(workspace.DialStdioCommand)
	if err != nil {
		done()
		return nil, err
	}
	// The lease is held for the life of the STREAM, not just the dial.
	//
	// Releasing it when the stream opens instead leaves a hijacked connection
	// (`docker attach`, `exec -it`, `logs -f`) pinning nothing at all. Most
	// survive anyway, but only indirectly, because their container is running
	// and hasLiveDependents notices it. A `logs -f` on a STOPPED container has
	// nothing holding the connection and is simply cut.
	//
	// This is also the reliable answer to "is anything using the connection".
	// A stream holds its lease for exactly as long as it is open; an idle
	// keep-alive connection between requests holds none, which is the
	// distinction that matters.
	return &leasedStream{ReadWriteCloser: stream, release: done}, nil
}

// leasedStream keeps a gate lease alive until the stream is closed.
type leasedStream struct {
	io.ReadWriteCloser
	release func()
	once    sync.Once
}

func (s *leasedStream) Close() error {
	err := s.ReadWriteCloser.Close()
	s.once.Do(s.release)
	return err
}

// CloseWrite forwards the half-close the hijack path depends on. Without it
// the wrapper would hide the method and `docker run` without -i would lose the
// container's output: the failure ADR 0005 records and the proxy's tests
// pin down.
func (s *leasedStream) CloseWrite() error {
	if cw, ok := s.ReadWriteCloser.(tunnel.WriteCloser); ok {
		return cw.CloseWrite()
	}
	return nil
}

// ContainerCreate satisfies proxy.Rewriter.
func (s *Session) ContainerCreate(ctx context.Context, body []byte) ([]byte, error) {
	live, done, err := s.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	return live.rewriter.ContainerCreate(ctx, body)
}

// Info returns what the workspace reports, connecting if necessary.
func (s *Session) Info(ctx context.Context) (workspace.Info, error) {
	live, done, err := s.acquire(ctx)
	if err != nil {
		return workspace.Info{}, err
	}
	defer done()
	return live.info, nil
}

// acquire returns the live connection, establishing one if needed.
func (s *Session) acquire(ctx context.Context) (*liveConn, func(), error) {
	s.wake()
	return s.gate.acquire(ctx)
}

// Standby releases the workspace but keeps the endpoint.
//
// What a reclaim is actually for: the connection is dropped and the file
// watches go, which on a large tree is the only local resource worth having
// back. The listener stays bound, so a foreign Docker client -- compose,
// buildx, Testcontainers, an IDE plugin -- connects and is served as before,
// and the next request rebuilds what this let go of.
func (s *Session) Standby() {
	s.dormantMu.Lock()
	already := s.dormant
	s.dormant = true
	s.dormantMu.Unlock()
	if already {
		return
	}

	if s.watch != nil {
		// An empty set removes every root; the watcher itself stays, so waking
		// is another Sync rather than a new watcher and a new observer.
		s.watch.Sync(nil)
	}
	s.gate.sweep(s.ctx)
	s.log().Info("standing by; the endpoint stays up")
}

// wake undoes Standby, and is called on the path every request takes.
func (s *Session) wake() {
	s.dormantMu.Lock()
	was := s.dormant
	s.dormant = false
	s.dormantMu.Unlock()
	if !was {
		return
	}

	s.syncWatch()
	s.log().Info("woken by a request")
}

// isDormant reports whether the workspace has been let go of.
func (s *Session) isDormant() bool {
	s.dormantMu.Lock()
	defer s.dormantMu.Unlock()
	return s.dormant
}

func readInfo(ctx context.Context, client *tunnelclient.Client) (workspace.Info, error) {
	out, err := client.Run(ctx, workspace.InfoCommand)
	if err != nil {
		return workspace.Info{}, fmt.Errorf("reading workspace info: %w", err)
	}
	return workspace.ParseInfo(bytes.NewReader(out))
}

// defaultAttrs is what every file in a share reports: the account as owner,
// wide bits so any uid a container runs as can write (ADR 0046).
func defaultAttrs() nfsserve.Attrs {
	return nfsserve.Attrs{
		FileMode: 0o666,
		DirMode:  0o777,
		// Windows has no execute bit to preserve, so without this nothing on
		// the share could be run. Elsewhere the real bits are used.
		AlwaysExecutable: runtime.GOOS == "windows",
	}
}

func attrsFor(info workspace.Info) nfsserve.Attrs {
	a := defaultAttrs()
	a.UID = uint32(info.UID)
	a.GID = uint32(info.GID)
	return a
}

// log is the session's logger, or silence. See logx.Or.
func (s *Session) log() *slog.Logger {
	return logx.Or(s.opts.Log)
}

// link is what the prefetch policy decides against: the round trip as last
// measured on this session. Bandwidth is left to the cache, which measures it
// from its own batches; nothing else here sends enough to know.
func (s *Session) link() dircache.Link {
	return dircache.Link{RTT: time.Duration(s.rtt.Load())}
}
