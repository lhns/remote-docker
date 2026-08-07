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

	Log Logger
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

	s.gate = &connGate[*liveConn]{
		open: s.connect,
		shut: func(live *liveConn) { live.close() },
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
		Shares:  shareRegistrar{s.registry},
		Volumes: live.api,
		NFSPort: info.NFSPort,
		Owner:   info.User,
	}
	live.nfs = nfsserve.New(s.registry)

	if err := s.startNFS(live); err != nil {
		_ = client.Close()
		return nil, err
	}

	liveCtx, cancel := context.WithCancel(s.ctx)
	live.cancel = cancel
	s.startPorts(liveCtx, live)

	live.wg.Go(func() {
		if _, err := s.collector(live).Collect(liveCtx); err != nil {
			s.logf("collecting unused share volumes: %v", err)
		}
	})

	s.logf("connected to %s@%s", s.opts.Config.User, s.opts.Config.Host)
	return live, nil
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
			s.logf("nfs server stopped: %v", err)
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
			s.logf("reconciling ports: %v", err)
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
// volumes has a live NFS mount that dropping the tunnel would break. A running
// container of ours may have published ports whose forwards exist only while
// we are connected.
func hasLiveDependents(ctx context.Context, live *liveConn) (bool, error) {
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

func (live *liveConn) close() {
	if live.cancel != nil {
		live.cancel()
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
func (s *Session) Shell(ctx context.Context) error {
	live, done, err := s.acquire(ctx)
	if err != nil {
		return err
	}
	defer done()
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
type shareRegistrar struct{ registry *nfsserve.Registry }

func (s shareRegistrar) Share(localPath string) (string, error) {
	share, err := s.registry.Register(localPath)
	if err != nil {
		return "", err
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
