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
	"net"
	"runtime"
	"sync"
	"time"

	"github.com/lhns/remote-docker/internal/client/config"
	"github.com/lhns/remote-docker/internal/client/fswatch"
	"github.com/lhns/remote-docker/internal/client/nfsserve"
	"github.com/lhns/remote-docker/internal/client/ports"
	"github.com/lhns/remote-docker/internal/client/proxy"
	"github.com/lhns/remote-docker/internal/client/rewrite"
	"github.com/lhns/remote-docker/internal/client/sshx"
	"github.com/lhns/remote-docker/pkg/workspace"
)

// Logger reports progress.
type Logger interface {
	Printf(format string, args ...any)
}

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

	// Role says what this session is for. See the constants: everything below
	// that used to be a separate switch is decided by it.
	Role Role

	// Watch replays this machine's filesystem changes into the workspace, so
	// watchers in containers notice them (ADR 0016). Off by default: nothing
	// is watched and no channel is opened.
	Watch        fswatch.Mode
	WatchBudget  int
	WatchExclude []string

	// Version is the build this session is running, reported to anything
	// asking whether it matches the client talking to it.
	Version string

	Log Logger
}

// Role is what a session is for. There are two, and the difference between
// them is three separate refusals that always travelled together.
type Role int

const (
	// Query only asks the workspace things -- `status`, `gc`. It binds
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
	// half-working when the export port is already taken -- two of them is a
	// genuine conflict and saying so beats a session that silently serves no
	// files.
	Host
)

// hosting is the single question the rest of this package asks. There is
// deliberately no serves()/exports()/narrates() trio: that would be three names
// for one bit, which is what this type replaced.
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

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	once   sync.Once
}

// liveConn is everything that exists only while connected.
type liveConn struct {
	ssh       *sshx.Client
	info      workspace.Info
	api       *proxy.APIClient
	rewriter  *rewrite.Rewriter
	nfs       *nfsserve.Server
	nfsTunnel net.Listener
	ports     *ports.Manager

	// notify is the change-notification channel, nil when the workspace does
	// not support it or watching is off.
	notify io.Closer

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

	if _, err := s.registry.RegisterCWD(opts.WorkDir); err != nil {
		cancel()
		return nil, err
	}

	if opts.Watch != fswatch.ModeOff && opts.Role.hosting() {
		watcher, err := fswatch.New(fswatch.Options{
			Mode:    opts.Watch,
			Budget:  opts.WatchBudget,
			Exclude: opts.WatchExclude,
			Log:     opts.Log,
		})
		if err != nil {
			cancel()
			return nil, err
		}
		s.watch = watcher
		s.watch.Sync(sharesOf(s.registry))
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
		idle: opts.IdleTimeout,
		log:  opts.Log,
	}

	s.started = time.Now()
	s.proxy = &proxy.Proxy{Dialer: s, Rewriter: s, Log: opts.Log}

	if opts.Role.hosting() {
		// Only a session that serves the endpoint answers for it. One that does
		// not would be claiming to be a daemon it is not.
		s.proxy.Control = s

		if err := s.listen(opts.Endpoint); err != nil {
			cancel()
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
			s.logf("docker endpoint stopped: %v", err)
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
	stream, err := live.ssh.OpenStream(proxy.DialStdioCommand)
	if err != nil {
		done()
		return nil, err
	}
	// The lease is held for the life of the STREAM, not just the dial.
	//
	// It used to be released the instant the stream opened, so a hijacked
	// connection -- `docker attach`, `exec -it`, `logs -f` -- held nothing at
	// all. Those survived an idle release only indirectly, because their
	// container was running and hasLiveDependents noticed it. A `logs -f` on a
	// STOPPED container had nothing pinning it and would simply be cut.
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
// container's output -- the failure ADR 0005 records and the proxy's tests
// pin down.
func (s *leasedStream) CloseWrite() error {
	if cw, ok := s.ReadWriteCloser.(interface{ CloseWrite() error }); ok {
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
	return s.gate.acquire(ctx)
}

func readInfo(ctx context.Context, client *sshx.Client) (workspace.Info, error) {
	out, err := client.Run(ctx, "workspace-info")
	if err != nil {
		return workspace.Info{}, fmt.Errorf("reading workspace info: %w", err)
	}
	return workspace.ParseInfo(bytes.NewReader(out))
}

func defaultAttrs() nfsserve.Attrs {
	return nfsserve.Attrs{
		FileMode: 0o644,
		DirMode:  0o755,
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

func (s *Session) logf(format string, args ...any) {
	if s.opts.Log != nil {
		s.opts.Log.Printf(format, args...)
	}
}
