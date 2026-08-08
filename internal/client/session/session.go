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
	"errors"
	"fmt"
	"io"
	"net"
	"os"
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

	// Progress enables the running commentary -- connected, forwarding a
	// port, watching. Off by default, and that default is the point: a
	// command's output belongs to the command. `remote-docker docker run`
	// prints a container's stdout, and our chatter interleaving with it is
	// noise in the success case. Only `up`, whose entire job is to hold a
	// session open and report on it, turns this on.
	//
	// Problems are reported either way, and always to stderr.
	Progress bool

	// Watch replays this machine's filesystem changes into the workspace, so
	// watchers in containers notice them (ADR 0016). Off by default: nothing
	// is watched and no channel is opened.
	Watch        fswatch.Mode
	WatchBudget  int
	WatchExclude []string

	// Version is the build this session is running, reported to anything
	// asking whether it matches the client talking to it.
	Version string

	// Serve says whether this session binds the local Docker endpoint.
	//
	// Off by default, and the default is the point. `status` and `gc` declined
	// the remote NFS port with some care and then bound the LOCAL endpoint
	// anyway, which they never use -- so on Windows, where the pipe bind is
	// genuinely exclusive, `status` could not run at all while `up` was
	// running. That is precisely when someone runs it.
	Serve bool

	// Files says whether this session should export the working directory.
	//
	// An account has exactly one reverse-tunnel port (ADR 0003), so only one
	// session at a time can serve files. A command that does not need to --
	// status, gc -- must not try, or it fails the moment `up` is running,
	// which is precisely when someone would run it.
	Files FileServing

	Log Logger
}

// DefaultIdleTimeout balances a reconnect against holding a connection nobody
// is using: long enough that someone working normally never notices, short
// enough that a workspace left open overnight is not holding anything.
const DefaultIdleTimeout = time.Minute

// FileServing says whether a session exports files, and how badly it needs to.
type FileServing int

const (
	// NoFiles does not export at all. For commands that only ask the daemon
	// questions.
	NoFiles FileServing = iota

	// FilesRequired fails when the port is taken. Two `up` sessions for one
	// account is a genuine conflict and reporting it beats half-working.
	FilesRequired
)

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

	if opts.Watch != fswatch.ModeOff && opts.Files != NoFiles {
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
	if opts.Serve {
		// Only a session that serves the endpoint answers for it. One that
		// does not would be claiming to be a daemon it is not.
		s.proxy.Control = s
	}

	if opts.Serve {
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

// connect brings up everything that needs the workspace. The order matters:
// the NFS export has to be reachable before any container can mount a volume
// backed by it.
func (s *Session) connect(ctx context.Context) (*liveConn, error) {
	key, err := sshx.LoadOrCreateKey(config.KeyPath(), config.KeyComment())
	if err != nil {
		return nil, err
	}
	known, err := sshx.NewKnownHosts(config.KnownHostsPath())
	if err != nil {
		return nil, err
	}

	client, err := sshx.Dial(ctx, sshx.Config{
		Host:       s.opts.Config.Host,
		Port:       s.opts.Config.Port,
		User:       s.opts.Config.User,
		Key:        key,
		KnownHosts: known,
	})
	if err != nil {
		return nil, err
	}

	info, err := readInfo(ctx, client)
	if err != nil {
		_ = client.Close()
		return nil, err
	}

	// Now the account is known, report its uid rather than the default, so
	// files are owned by whoever will read them.
	s.registry.SetAttrs(attrsFor(info))

	live := &liveConn{ssh: client, info: info}
	live.api = &proxy.APIClient{Dialer: &proxy.SSHDialer{Client: client}}
	live.rewriter = &rewrite.Rewriter{
		Shares:  shareRegistrar{registry: s.registry, changed: s.sharesChanged},
		Volumes: live.api,
		NFSPort: info.NFSPort,
		Owner:   info.User,
	}
	if s.opts.Files != NoFiles {
		live.nfs = nfsserve.New(s.registry)
		if err := s.startNFS(live); err != nil {
			_ = client.Close()
			return nil, err
		}
	}

	liveCtx, cancel := context.WithCancel(s.ctx)
	live.cancel = cancel

	// Both of these belong to a session that is HOSTING something: forwarding
	// ports exists to make this session's containers reachable, and collecting
	// volumes is housekeeping for a long-running `up`.
	//
	// A NoFiles session -- `status`, `gc` -- only asks the workspace a
	// question and then closes. Starting them there meant a status command
	// began two background round trips and immediately tore the connection out
	// from under them, so it printed its table followed by two errors about
	// work the user never asked for.
	if s.opts.Files != NoFiles {
		s.startPorts(liveCtx, live)
		s.startNotify(live)

		live.wg.Go(func() {
			if _, err := s.collector(live).Collect(liveCtx); err != nil {
				s.logQuiet(liveCtx, "collecting unused share volumes: %v", err)
			}
		})
	}

	s.progressf("connected to %s@%s", s.opts.Config.User, s.opts.Config.Host)
	return live, nil
}

// shareReconcileInterval matches the port manager's: the same reasoning
// applies, that a direct notification can be missed and a cheap periodic pass
// covers it.
const shareReconcileInterval = 30 * time.Second

// sharesChanged tells the watcher a share was just registered.
func (s *Session) sharesChanged() {
	if s.watch != nil {
		s.watch.Sync(sharesOf(s.registry))
	}
}

// logQuiet reports an error unless the context that owns the work has already
// been cancelled.
//
// Everything here talks over one SSH connection, so tearing that connection
// down makes every goroutine still using it fail at once -- with EOF, or
// "unexpected packet in response to channel open", or a half-read stream.
// Those are descriptions of shutdown, not of anything wrong, and printing them
// after the user pressed Ctrl-C or after a one-shot command finished is how a
// clean exit came to look like a crash.
// progressf reports routine progress, which most commands do not want.
func (s *Session) progressf(format string, args ...any) {
	if s.opts.Progress {
		s.logf(format, args...)
	}
}

// portsLogger is the logger the port manager gets: the real one when progress
// is wanted, and nil otherwise, because "forwarding ..." arriving in the
// middle of a container's output is exactly the pollution this avoids.
func (s *Session) portsLogger() ports.Logger {
	if s.opts.Progress {
		return s.opts.Log
	}
	return nil
}

func (s *Session) logQuiet(ctx context.Context, format string, args ...any) {
	if ctx.Err() != nil || s.ctx.Err() != nil {
		return
	}
	s.logf(format, args...)
}

func (s *Session) startNFS(live *liveConn) error {
	addr := net.JoinHostPort("127.0.0.1", fmt.Sprint(live.info.NFSPort))

	l, err := live.ssh.Listen(addr)
	if err != nil {
		return fmt.Errorf("reserving %s on the workspace: %w "+
			"(another session for this account may still be open)", addr, err)
	}
	live.nfsTunnel = l

	live.wg.Go(func() {
		if err := live.nfs.Serve(l); err != nil && !errors.Is(err, net.ErrClosed) {
			s.logQuiet(s.ctx, "nfs server stopped: %v", err)
		}
	})
	return nil
}

func (s *Session) startPorts(ctx context.Context, live *liveConn) {
	live.ports = &ports.Manager{
		Docker:    dockerPorts{live.api},
		Forwarder: sshForwarder{live.ssh},
		Log:       s.portsLogger(),
		Owned: func(c ports.Container) bool {
			return c.Labels[rewrite.OwnerLabel] == live.info.User
		},
	}
	live.wg.Go(func() { s.watchPorts(ctx, live) })
}

// watchPorts reconciles on container events and on a timer. The timer is not
// redundant: the event stream can drop, and a container whose ports are
// silently unreachable is worse than a cheap periodic pass.
func (s *Session) watchPorts(ctx context.Context, live *liveConn) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	reconcile := func() {
		if err := live.ports.Reconcile(ctx); err != nil {
			s.logQuiet(ctx, "reconciling ports: %v", err)
		}
	}
	reconcile()

	for ctx.Err() == nil {
		events, closer, err := live.api.Events(ctx)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				reconcile()
			}
			continue
		}

		func() {
			defer func() { _ = closer.Close() }()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					reconcile()
				case event, ok := <-events:
					if !ok {
						return
					}
					switch event.Action {
					case "start", "die", "destroy", "stop", "kill":
						reconcile()
					}
				}
			}
		}()

		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

// sweepIdle releases the connection when nothing needs it.
func (s *Session) sweepIdle() {
	interval := max(s.opts.IdleTimeout/2, time.Second)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.releaseIfIdle()
		}
	}
}

func (s *Session) releaseIfIdle() {
	s.gate.sweep(s.ctx)
}

// hasLiveDependents reports whether anything running still needs us.
//
// Two things can, and both must be checked. A container holding one of our
// volumes has a live NFS mount that dropping the tunnel would break, and a
// running container of ours may have published ports whose forwards exist only
// while we are connected.
//
// A third used to be listed here -- an interactive shell using the ~/workspace
// mount -- and was counted separately on liveConn. It never reached this
// function: Shell holds its gate lease for its whole life, and sweep bails on
// users > 0 long before busy is consulted. Now that every stream holds its
// lease the same way, the counter documented an intent the code no longer
// needed, so it is gone.
//
// The volume match is scoped to volumes WE created. It used to accept any
// rd- prefix, so on a shared daemon (ADR 0012) another account's volume pinned
// this connection open forever -- an idle release that could never fire, for a
// dependency that was not ours.
func (s *Session) hasLiveDependents(ctx context.Context, live *liveConn) (bool, error) {
	containers, err := live.api.ListContainers(ctx)
	if err != nil {
		return false, err
	}
	ours := s.ourVolumes()
	for _, c := range containers {
		if c.Labels[rewrite.OwnerLabel] == live.info.User {
			return true, nil
		}
		for _, m := range c.Mounts {
			if m.Type == "volume" && ours[m.Name] {
				return true, nil
			}
		}
	}
	return false, nil
}

// ourVolumes names the volumes backing this session's shares.
//
// Derived rather than remembered: share ids are a pure function of the local
// path (ADR 0007), so the registry already knows the exact set and no round
// trip is needed to ask.
func (s *Session) ourVolumes() map[string]bool {
	shares := s.registry.Shares()
	out := make(map[string]bool, len(shares))
	for _, share := range shares {
		if name, err := workspace.VolumeNameForExport(share.ExportPath); err == nil {
			out[name] = true
		}
	}
	return out
}

func (live *liveConn) close() {
	if live.cancel != nil {
		live.cancel()
	}
	if live.notify != nil {
		_ = live.notify.Close()
	}
	if live.ports != nil {
		_ = live.ports.Close()
	}
	if live.nfsTunnel != nil {
		_ = live.nfsTunnel.Close()
	}
	_ = live.ssh.Close()
	live.wg.Wait()
}

// Collect removes share volumes this account is no longer using.
func (s *Session) Collect(ctx context.Context) (int, error) {
	live, done, err := s.acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer done()
	return s.collector(live).Collect(ctx)
}

func (s *Session) collector(live *liveConn) *rewrite.Collector {
	return &rewrite.Collector{
		Volumes: live.api,
		Remover: live.api,
		InUse:   live.api,
		Owner:   live.info.User,
		Log:     s.opts.Log,
	}
}

// Status answers the control endpoint, satisfying proxy.Control.
//
// Deliberately does NOT connect. `status` connecting is its own decision --
// reporting what the workspace says is that command's whole job -- but a
// daemon asked to describe itself must not go and establish a connection it
// had let go, which would make asking the question change the answer.
func (s *Session) Status() any {
	live, connected := s.gate.current()
	st := proxy.Status{
		Version:   s.opts.Version,
		Workspace: s.opts.Config.Name,
		Host:      s.opts.Config.Host,
		User:      s.opts.Config.User,
		Endpoint:  s.Endpoint,
		PID:       os.Getpid(),
		Connected: connected,
		Since:     s.started.Format(time.RFC3339),
	}
	if connected {
		st.User = live.info.User
		st.Ports = s.Ports()
	}
	if s.watch != nil {
		st.Watching = s.watch.Stats().Mode.String()
	}
	for _, share := range s.registry.Shares() {
		st.Shares = append(st.Shares, share.LocalPath)
	}
	return st
}

// Idle reports whether this session could be ended without breaking anything,
// satisfying proxy.Control.
func (s *Session) Idle() any {
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()

	quiet, safe := s.IdleFor(ctx)
	return proxy.Idle{Safe: safe, Quiet: quiet.Round(time.Second).String()}
}

// Shutdown asks the session to stop, satisfying proxy.Control.
//
// Returns immediately and stops in the background, because the caller is the
// control request still holding a connection that Close is about to shut.
func (s *Session) Shutdown() {
	go func() {
		s.stopOnce.Do(func() { close(s.stopped) })
	}()
}

// IdleFor reports how long this session has had nothing to do, and whether it
// would be safe to end the process now.
//
// Safe means the same thing it means for releasing a connection, because the
// consequence is worse: a released connection reopens on the next request, and
// an ended process takes the NFS export with it and a running container's
// filesystem with that.
//
// The disjunction is the load-bearing part. If no connection is held, the gate
// only let it go BECAUSE nothing depended on it, so there is nothing to ask
// and nothing to break. If one is held, ask -- and "unable to tell" counts as
// busy, exactly as it does for a release.
func (s *Session) IdleFor(ctx context.Context) (time.Duration, bool) {
	last, inUse := s.gate.lastUse()
	if inUse {
		return 0, false
	}
	// Never used means idle since the session began, not idle for no time at
	// all. Reading the zero time as "just now" meant a daemon that had served
	// nothing could never expire -- the one case where reclaiming it is most
	// obviously right, and the case `start` leaves behind every time somebody
	// opens a session and then does not use it.
	if last.IsZero() {
		last = s.started
	}
	quiet := time.Since(last)

	live, connected := s.gate.current()
	if !connected {
		return quiet, true
	}

	busy, err := s.hasLiveDependents(ctx, live)
	if err != nil || busy {
		return quiet, false
	}
	return quiet, true
}

// Stopped is closed when something has asked this session to stop. `up` waits
// on it alongside its signal context.
func (s *Session) Stopped() <-chan struct{} { return s.stopped }

// Ports lists the ports currently forwarded, if connected.
func (s *Session) Ports() []int {
	live, ok := s.gate.current()
	if !ok || live.ports == nil {
		return nil
	}
	return live.ports.Active()
}

// Close tears the session down.
func (s *Session) Close() error {
	if s.watch != nil {
		_ = s.watch.Close()
	}
	s.once.Do(func() {
		s.cancel()
		if s.listener != nil {
			_ = s.listener.Close()
		}
		s.gate.close()
		s.wg.Wait()
	})
	return nil
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

// shareRegistrar adapts the NFS registry to the rewriter's Sharer.
//
// It is also where a newly shared directory becomes a watched one: every bind
// rewrite funnels through here, so the watcher learns about a share the moment
// it exists rather than up to a reconcile interval later.
type shareRegistrar struct {
	registry *nfsserve.Registry
	changed  func()
}

func (s shareRegistrar) Share(localPath string) (string, error) {
	share, err := s.registry.Register(localPath)
	if err != nil {
		return "", err
	}
	if s.changed != nil {
		s.changed()
	}
	return share.ExportPath, nil
}

// sshForwarder adapts the SSH client to the port manager's Forwarder.
type sshForwarder struct{ client *sshx.Client }

func (f sshForwarder) Forward(local, remote string) (ports.Forward, error) {
	fwd, err := f.client.Forward(local, remote)
	if err != nil {
		return nil, err
	}
	return forwardAdapter{fwd}, nil
}

type forwardAdapter struct{ fwd *sshx.Forward }

func (f forwardAdapter) Close() error        { return f.fwd.Close() }
func (f forwardAdapter) LocalAddr() net.Addr { return f.fwd.Local }

// dockerPorts adapts the API client to the port manager's Docker.
type dockerPorts struct{ api *proxy.APIClient }

func (d dockerPorts) ListContainers(ctx context.Context) ([]ports.Container, error) {
	raw, err := d.api.ListContainers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ports.Container, 0, len(raw))
	for _, c := range raw {
		container := ports.Container{ID: c.ID, Labels: c.Labels}
		if len(c.Names) > 0 {
			container.Name = c.Names[0]
		}
		for _, p := range c.Ports {
			container.Ports = append(container.Ports, ports.Published{
				PublicPort:  p.PublicPort,
				PrivatePort: p.PrivatePort,
				Type:        p.Type,
			})
		}
		out = append(out, container)
	}
	return out, nil
}
