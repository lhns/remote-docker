// Package session wires the client's parts into one live connection to a
// workspace: the SSH transport, the NFS export behind a reverse forward, the
// Docker API proxy, and the port forwards.
package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
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

	// WorkDir is the directory exported at /cwd. Usually the process's
	// working directory.
	WorkDir string

	// Endpoint overrides where the Docker API is served locally.
	Endpoint string

	Log Logger
}

// Session is a live workspace connection.
type Session struct {
	Info     workspace.Info
	Endpoint string

	ssh      *sshx.Client
	nfs      *nfsserve.Server
	registry *nfsserve.Registry
	proxy    *proxy.Proxy
	api      *proxy.APIClient
	pm       *ports.Manager

	listener  net.Listener
	nfsTunnel net.Listener

	log  Logger
	once sync.Once
	wg   sync.WaitGroup
}

// Open connects to the workspace and brings up every moving part.
//
// The order matters: the NFS export has to be reachable before any container
// can mount a volume backed by it, so the reverse forward is established and
// serving before the Docker endpoint accepts a single request.
func Open(ctx context.Context, opts Options) (*Session, error) {
	if err := opts.Config.RequireHost(); err != nil {
		return nil, err
	}

	key, err := sshx.LoadOrCreateKey(config.KeyPath(), keyComment())
	if err != nil {
		return nil, err
	}
	known, err := sshx.NewKnownHosts(config.KnownHostsPath())
	if err != nil {
		return nil, err
	}

	client, err := sshx.Dial(ctx, sshx.Config{
		Host:       opts.Config.Host,
		Port:       opts.Config.Port,
		User:       opts.Config.User,
		Key:        key,
		KnownHosts: known,
	})
	if err != nil {
		return nil, err
	}

	s := &Session{ssh: client, log: opts.Log}

	info, err := s.readInfo(ctx)
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	s.Info = info

	// Report the workspace account's own uid, so files under the export are
	// owned by the account that will read them rather than by uid 1000.
	s.registry = nfsserve.NewRegistry(nfsserve.Attrs{
		UID:      uint32(info.UID),
		GID:      uint32(info.GID),
		FileMode: 0o644,
		DirMode:  0o755,
	})
	if _, err := s.registry.RegisterCWD(opts.WorkDir); err != nil {
		_ = client.Close()
		return nil, err
	}
	s.nfs = nfsserve.New(s.registry)

	if err := s.startNFS(info.NFSPort); err != nil {
		_ = client.Close()
		return nil, err
	}

	s.api = &proxy.APIClient{Dialer: &proxy.SSHDialer{Client: client}}
	s.proxy = &proxy.Proxy{
		Dialer: &proxy.SSHDialer{Client: client},
		Rewriter: &rewrite.Rewriter{
			Shares:  shareRegistrar{s.registry},
			Volumes: s.api,
			NFSPort: info.NFSPort,
			Owner:   info.User,
		},
		Log: opts.Log,
	}

	if err := s.startProxy(ctx, opts.Endpoint); err != nil {
		_ = s.Close()
		return nil, err
	}

	s.startPorts(ctx)
	return s, nil
}

// readInfo asks the workspace for this account's parameters.
func (s *Session) readInfo(ctx context.Context) (workspace.Info, error) {
	out, err := s.ssh.Run(ctx, "workspace-info")
	if err != nil {
		return workspace.Info{}, fmt.Errorf("reading workspace info: %w", err)
	}
	return workspace.ParseInfo(newReader(out))
}

// startNFS asks the workspace to listen on this account's port and serves the
// export behind it.
func (s *Session) startNFS(port int) error {
	addr := net.JoinHostPort("127.0.0.1", fmt.Sprint(port))

	l, err := s.ssh.Listen(addr)
	if err != nil {
		// The workspace refuses a port that is not this account's, and refuses
		// one already bound by an earlier session that has not yet been
		// cleaned up. Both look the same from here, so say so.
		return fmt.Errorf("reserving %s on the workspace: %w "+
			"(another session for this account may still be open)", addr, err)
	}
	s.nfsTunnel = l

	s.wg.Go(func() {
		if err := s.nfs.Serve(l); err != nil && !errors.Is(err, net.ErrClosed) {
			s.logf("nfs server stopped: %v", err)
		}
	})
	s.logf("exporting %s at 127.0.0.1:%d on the workspace", s.registry.Shares()[0].LocalPath, port)
	return nil
}

func (s *Session) startProxy(ctx context.Context, endpoint string) error {
	l, err := proxy.Listen(endpoint)
	if err != nil {
		return err
	}
	s.listener = l
	s.Endpoint = proxy.DockerHost(endpoint)

	s.wg.Go(func() {
		if err := s.proxy.Serve(ctx, l); err != nil {
			s.logf("docker endpoint stopped: %v", err)
		}
	})
	return nil
}

// startPorts keeps local forwards in step with the containers we started.
func (s *Session) startPorts(ctx context.Context) {
	s.pm = &ports.Manager{
		Docker:    dockerPorts{s.api},
		Forwarder: sshForwarder{s.ssh},
		Log:       s.log,
		Owned: func(c ports.Container) bool {
			return c.Labels[rewrite.OwnerLabel] == s.Info.User
		},
	}

	s.wg.Go(func() { s.watchPorts(ctx) })
}

// watchPorts reconciles on every container event, and periodically regardless.
//
// The periodic pass is not redundant: the event stream can drop, and a
// reconciliation is cheap next to a container whose ports are silently
// unreachable until the next thing happens to start.
func (s *Session) watchPorts(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	reconcile := func() {
		if err := s.pm.Reconcile(ctx); err != nil {
			s.logf("reconciling ports: %v", err)
		}
	}
	reconcile()

	for {
		events, closer, err := s.api.Events(ctx)
		if err != nil {
			// The stream is unavailable; the ticker still keeps forwards
			// roughly correct, so this is not fatal.
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				reconcile()
				continue
			}
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
						return // stream ended; reconnect below
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

// Close tears the session down.
func (s *Session) Close() error {
	var err error
	s.once.Do(func() {
		if s.pm != nil {
			_ = s.pm.Close()
		}
		if s.listener != nil {
			_ = s.listener.Close()
		}
		if s.nfsTunnel != nil {
			_ = s.nfsTunnel.Close()
		}
		if s.ssh != nil {
			err = s.ssh.Close()
		}
		s.wg.Wait()
	})
	return err
}

// Shell opens an interactive session on the workspace.
func (s *Session) Shell(ctx context.Context) error {
	return s.ssh.Shell(ctx, "cd ~/workspace 2>/dev/null; exec ${SHELL:-/bin/sh} -l")
}

// Ports lists the ports currently forwarded to this machine.
func (s *Session) Ports() []int {
	if s.pm == nil {
		return nil
	}
	return s.pm.Active()
}

func (s *Session) logf(format string, args ...any) {
	if s.log != nil {
		s.log.Printf(format, args...)
	}
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

// newReader adapts command output for the info parser.
func newReader(b []byte) io.Reader { return bytes.NewReader(b) }

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
