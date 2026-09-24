// Package session wires the client's parts into one live connection to a
// workspace: the SSH transport, the NFS export behind a reverse forward, the
// Docker API proxy, and the port forwards. The endpoint stays bound; the
// connection behind it opens on first use and is released when idle.
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

	// Endpoint overrides where the Docker API is served locally.
	Endpoint string

	// IdleTimeout is how long the workspace connection may sit unused before
	// being released. Zero uses DefaultIdleTimeout; negative never releases.
	IdleTimeout time.Duration

	Role Role

	// Watch replays this machine's filesystem changes into the workspace
	// (ADR 0016). Off by default.
	Watch        fswatch.Mode
	WatchBudget  int
	WatchExclude []string

	// Mode is what a share gets on each axis the mount left unset, and
	// ModePaths overrides it per directory (ADR 0042).
	Mode      workspace.Mode
	ModePaths map[string]workspace.Mode

	// PosixSource reports the POSIX path a shell may have rewritten a bind
	// source into. Nil except on Windows.
	PosixSource func(source string) string

	// Version is this build, reported to a client checking for a mismatch.
	Version string

	Log *slog.Logger
}

// Role is what a session is for.
type Role int

const (
	// Query only asks, as `status` and `gc` do, and is silent. It must not bind
	// the endpoint (a Windows pipe bind excludes, so `status` could not run
	// beside a session) nor export files (it would take the reverse-tunnel
	// port a real session needs).
	Query Role = iota

	// Host is what `start` runs: it binds the endpoint, exports, forwards
	// ports and narrates. It fails outright when the export port is taken.
	Host
)

// hosting is the one question asked of a Role, so its behaviours cannot
// disagree.
func (r Role) hosting() bool { return r == Host }

func (r Role) String() string {
	if r == Host {
		return "host"
	}
	return "query"
}

// DefaultIdleTimeout is how long an unused connection is kept.
const DefaultIdleTimeout = time.Minute

// Session serves the local Docker endpoint for one workspace.
type Session struct {
	Endpoint string

	opts     Options
	listener net.Listener
	proxy    *proxy.Proxy

	// registry, nfs and watch outlive every connection. nfs above all: the
	// kernel keeps presenting the handles it was given, so a server rebuilt per
	// connection leaves every running container with "Stale file handle" on a
	// mount that looks fine (test/nfs-resilience.sh section 10).
	registry *nfsserve.Registry
	nfs      *nfsserve.Server

	// clientID names this machine, derived from its key on the first connect.
	clientID string

	// shares and cache are nil on a query session, which restores and caches
	// nothing.
	shares *shareStore
	cache  *dircache.Cache // ADR 0044

	// rtt is the tunnel's last measured round trip in nanoseconds, for prefetch.
	rtt atomic.Int64

	watch      *fswatch.Watcher
	started    time.Time
	notifyOnce sync.Once

	gate *connGate[*liveConn]

	stopped  chan struct{}
	stopOnce sync.Once

	// dormant is set by Standby and cleared by the next request.
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

	// clockSkew is the workspace's clock minus this machine's, used to decide
	// which side wrote last in a write-back conflict (ADR 0044).
	clockSkew time.Duration

	// notify is nil when the workspace lacks it or watching is off.
	notify io.Closer

	// cacheChan is opened on first use and only once, so a workspace too old
	// for it is not asked per container. cacheErr says why there is none.
	cacheOnce sync.Once
	cacheChan *cacheChannel
	cacheErr  error

	// machine holds a local machine open; nil for a remote workspace.
	machine io.Closer

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// Open binds the local Docker endpoint. It connects on the first request.
func Open(ctx context.Context, opts Options) (*Session, error) {
	if err := opts.Config.RequireHost(); err != nil {
		return nil, err
	}
	if opts.IdleTimeout == 0 {
		opts.IdleTimeout = DefaultIdleTimeout
	}

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))

	// go-nfs otherwise logs straight to the user's terminal.
	nfsserve.SetLogger(opts.Log)

	s := &Session{
		opts:    opts,
		ctx:     runCtx,
		cancel:  cancel,
		stopped: make(chan struct{}),
		// Corrected once the workspace reports its uid, before anything is served.
		registry: nfsserve.NewRegistry(defaultAttrs()),
	}
	// Set before the first share is registered, whose filesystem is built with it.
	s.registry.Log = opts.Log

	// One list for both: a directory the watcher skips but the cache fills
	// would be served stale for good.
	exclude := fswatch.ExcludesOr(opts.WatchExclude)

	if opts.Role.hosting() {
		s.shares = newShareStore(config.SharesPath(opts.Config.Name), opts.Log)
		s.registry.Restore = s.shares.restore
		s.registry.Recorded = s.shares.exports
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
		s.registry.OnRead = s.cache.Touch
		s.nfs = nfsserve.New(s.registry, opts.Log)
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
		// Sees every change before the mode filters any: a deletion the cache
		// misses leaves it serving a file that is gone (ADR 0044).
		s.watch.SetObserver(cacheObserver{cache: s.cache})
		s.syncWatch()
		s.wg.Go(func() { s.reconcileShares(runCtx, shareReconcileInterval) })
	}

	s.gate = &connGate[*liveConn]{
		open: s.connect,
		shut: func(live *liveConn) {
			// The watches stay; what is missed meanwhile is announced on the
			// next connection.
			if s.watch != nil {
				s.watch.ClearSink()
			}
			live.close()
		},
		busy:  s.hasLiveDependents,
		alive: func(live *liveConn) bool { return live.ssh.Alive() },
		idle:  opts.IdleTimeout,
		log:   opts.Log,
	}

	s.started = time.Now()
	s.proxy = &proxy.Proxy{Dialer: s, Rewriter: s, Log: opts.Log}

	if opts.Role.hosting() {
		s.proxy.Control = s

		if err := s.listen(opts.Endpoint); err != nil {
			cancel()
			// The watcher holds handles the context does not.
			if s.watch != nil {
				_ = s.watch.Close()
			}
			return nil, err
		}
	} else {
		// Commands still print where the endpoint would be.
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

// DialDocker satisfies proxy.Dialer. Every request connects through here.
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
	// The lease lives as long as the stream: otherwise `attach`, `exec -it` and
	// `logs -f` pin nothing, and a `logs -f` on a stopped container is cut by
	// the idle release. It also tells a stream in use from an idle keep-alive.
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

// CloseWrite forwards the half-close; hiding it loses the output of `docker
// run` without -i (ADR 0005).
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

func (s *Session) acquire(ctx context.Context) (*liveConn, func(), error) {
	s.wake()
	return s.gate.acquire(ctx)
}

// Standby drops the connection and the file watches but keeps the endpoint
// bound, so any Docker client is still served and the next request rebuilds
// what this let go of.
func (s *Session) Standby() {
	s.dormantMu.Lock()
	already := s.dormant
	s.dormant = true
	s.dormantMu.Unlock()
	if already {
		return
	}

	if s.watch != nil {
		// Removes every root but keeps the watcher, so waking is another Sync.
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

func (s *Session) isDormant() bool {
	s.dormantMu.Lock()
	defer s.dormantMu.Unlock()
	return s.dormant
}

// readInfo asks the workspace what it is, bounded because the caller's context
// often has no deadline.
func readInfo(ctx context.Context, client *tunnelclient.Client) (workspace.Info, error) {
	ctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()

	out, err := client.Run(ctx, workspace.InfoCommand)
	if err != nil {
		return workspace.Info{}, fmt.Errorf("reading workspace info: %w", err)
	}
	return workspace.ParseInfo(bytes.NewReader(out))
}

// defaultAttrs is what every file in a share reports (ADR 0046).
func defaultAttrs() nfsserve.Attrs {
	a := nfsserve.DefaultAttrs
	// Windows has no execute bit, so without this nothing on a share runs.
	a.AlwaysExecutable = runtime.GOOS == "windows"
	return a
}

func attrsFor(info workspace.Info) nfsserve.Attrs {
	a := defaultAttrs()
	a.UID = uint32(info.UID)
	a.GID = uint32(info.GID)
	return a
}

func (s *Session) log() *slog.Logger {
	return logx.Or(s.opts.Log)
}

// link is the round trip for the prefetch policy; the cache measures bandwidth
// itself.
func (s *Session) link() dircache.Link {
	return dircache.Link{RTT: time.Duration(s.rtt.Load())}
}
