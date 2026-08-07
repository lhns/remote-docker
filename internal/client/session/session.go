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
	"sync/atomic"
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

	// Watch replays this machine's filesystem changes into the workspace, so
	// watchers in containers notice them (ADR 0016). Off by default: nothing
	// is watched and no channel is opened.
	Watch        fswatch.Mode
	WatchBudget  int
	WatchExclude []string

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

	// FilesIfAvailable exports when the port is free and carries on when it is
	// not, because another session already serving means the files are
	// already there.
	FilesIfAvailable

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
	notifyOnce sync.Once

	gate *connGate[*liveConn]

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

	// sessions counts interactive shells open on this connection.
	sessions atomic.Int64

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
		opts:   opts,
		ctx:    runCtx,
		cancel: cancel,
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
		busy: hasLiveDependents,
		idle: opts.IdleTimeout,
		log:  opts.Log,
	}

	s.proxy = &proxy.Proxy{Dialer: s, Rewriter: s, Log: opts.Log}

	if err := s.listen(opts.Endpoint); err != nil {
		cancel()
		return nil, err
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
	defer done()
	return live.ssh.OpenStream(proxy.DialStdioCommand)
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
	key, err := sshx.LoadOrCreateKey(config.KeyPath(), keyComment())
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
			if s.opts.Files == FilesRequired {
				_ = client.Close()
				return nil, err
			}
			// Another session holds the port, which means it is already
			// exporting this account's files. Nothing to do and nothing wrong.
			s.logf("not exporting files: %v", err)
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

	s.logf("connected to %s@%s", s.opts.Config.User, s.opts.Config.Host)
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
		Log:       s.opts.Log,
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
// Three things can, and all must be checked. A container holding one of our
// volumes has a live NFS mount that dropping the tunnel would break. A running
// container of ours may have published ports whose forwards exist only while
// we are connected. And an interactive session is using the ~/workspace mount,
// which the agent unmounts when this connection's forward is released -- so
// releasing while somebody sits at a shell pulls their working directory out
// from under them.
//
// The third was missed at first, because a session is not a container and the
// check only looked at containers. Same mistake as sessions reserving the
// export port they did not need: reasoning about one session at a time rather
// than several coexisting.
func hasLiveDependents(ctx context.Context, live *liveConn) (bool, error) {
	if live.sessions.Load() > 0 {
		return true, nil
	}
	containers, err := live.api.ListContainers(ctx)
	if err != nil {
		return false, err
	}
	for _, c := range containers {
		if c.Labels[rewrite.OwnerLabel] == live.info.User {
			return true, nil
		}
		for _, m := range c.Mounts {
			if m.Type == "volume" && workspace.IsManagedVolume(m.Name) {
				return true, nil
			}
		}
	}
	return false, nil
}

// OwnedVolumesInUse counts running containers holding a share this session
// serves.
//
// For a session that is about to end: those containers keep running, but their
// mounts are served by this process and stop working when it goes. Reported
// rather than prevented, because refusing to exit would be worse.
func (s *Session) OwnedVolumesInUse(ctx context.Context) (int, error) {
	live, ok := s.gate.current()
	if !ok {
		return 0, nil
	}
	containers, err := live.api.ListContainers(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, c := range containers {
		for _, m := range c.Mounts {
			if m.Type == "volume" && workspace.IsManagedVolume(m.Name) {
				n++
				break
			}
		}
	}
	return n, nil
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

// Shell opens an interactive session on the workspace.
//
// Counted for the whole of its life, not just while acquiring: a shell can sit
// idle for a long time and still be very much in use, and the idle sweep would
// otherwise release the connection and unmount the workspace underneath it.
func (s *Session) Shell(ctx context.Context) error {
	live, done, err := s.acquire(ctx)
	if err != nil {
		return err
	}
	defer done()

	live.sessions.Add(1)
	defer live.sessions.Add(-1)

	return live.ssh.Shell(ctx, "cd ~/workspace 2>/dev/null; exec ${SHELL:-/bin/sh} -l")
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

// Ports lists the ports currently forwarded, if connected.
func (s *Session) Ports() []int {
	live, ok := s.gate.current()
	if !ok || live.ports == nil {
		return nil
	}
	return live.ports.Active()
}

// Connected reports whether a workspace connection is currently open.
func (s *Session) Connected() bool {
	_, ok := s.gate.current()
	return ok
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

// keyComment identifies this machine in the enrolled public key.
func keyComment() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	user := os.Getenv("USER")
	if user == "" {
		user = os.Getenv("USERNAME")
	}
	if user == "" {
		return "remote-docker-" + host
	}
	return fmt.Sprintf("remote-docker-%s-%s", host, user)
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
