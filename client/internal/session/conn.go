package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/lhns/remote-docker/client/internal/config"
	"github.com/lhns/remote-docker/client/internal/ports"
	"github.com/lhns/remote-docker/client/internal/proxy"
	"github.com/lhns/remote-docker/client/internal/rewrite"
	"github.com/lhns/remote-docker/core-client/keys"
	"github.com/lhns/remote-docker/core-client/tunnelclient"
	"github.com/lhns/remote-docker/machine"

	"github.com/lhns/remote-docker/core/workspace"
)

// connect brings up everything that needs the workspace. The NFS export must be
// reachable before any container can mount a volume backed by it.
func (s *Session) connect(ctx context.Context) (*liveConn, error) {
	key, err := keys.LoadOrCreateKey(config.KeyPath(), config.KeyComment())
	if err != nil {
		return nil, err
	}

	hostKey, err := hostKeyRule(s.opts.Config)
	if err != nil {
		return nil, err
	}

	client, hold, err := dial(ctx, s.opts.Config, key.Signer, hostKey)
	if err != nil {
		return nil, err
	}
	// Released on any failure until live takes it over.
	defer func() {
		if hold != nil {
			_ = hold.Close()
		}
	}()

	if err := s.announceRun(ctx, client); err != nil {
		_ = client.Close()
		return nil, err
	}

	// Timed: one request and one reply is the round trip prefetch decides on.
	started := time.Now()
	info, err := readInfo(ctx, client)
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	s.rtt.Store(int64(time.Since(started)))
	s.clientID = clientIDFor(info, key.Signer.PublicKey().Marshal())

	s.registry.SetAttrs(attrsFor(info))

	live := &liveConn{ssh: client, info: info, machine: hold}
	hold = nil
	if info.Now != 0 {
		live.clockSkew = time.Duration(info.Now - time.Now().UnixNano())
	}
	live.api = &proxy.APIClient{Dialer: &proxy.SSHDialer{Client: client}}
	live.guard = &rewrite.Guard{Exported: s.exportsVolume}
	live.rewriter = &rewrite.Rewriter{
		Shares:   shareRegistrar{registry: s.registry, shares: s.shares, changed: s.syncWatch},
		Volumes:  live.api,
		NFSPort:  info.NFSPort,
		NConnect: s.nconnect(),
		Owner:    info.User,
		Client:   s.clientID,
		Guard:    live.guard,

		Mode:      s.opts.Mode,
		ModePaths: s.opts.ModePaths,
		Watching:  s.watch != nil,

		OpenCache:  func(ctx context.Context) (rewrite.Cache, error) { return s.shareCacheFor(ctx, live) },
		UnionReady: info.Union,

		DockerVersion: info.Docker,
		DaemonPaths:   info.DaemonPaths,
		PosixSource:   s.opts.PosixSource,
		LocalPortFree: func(port int) error { return localPortFree(live, port) },
	}
	if s.opts.Role.hosting() {
		if err := s.startNFS(live); err != nil {
			// live.close, not just the ssh client's: it releases the machine.
			live.close()
			return nil, err
		}
		live.rewriter.NFSPort = live.info.NFSPort // a run's, chosen by the bind
	}

	liveCtx, cancel := context.WithCancel(s.ctx)
	live.cancel = cancel

	// A query session closes right after its answer, so background work
	// started there only prints errors about work nobody asked for.
	if s.opts.Role.hosting() {
		s.startPorts(liveCtx, live)
		s.startNotify(live)

		// Idle until a share has a complete cache (ADR 0044).
		live.wg.Go(func() { s.cache.WriteBack(liveCtx) })

		live.wg.Go(func() {
			if _, err := s.collector(live).Collect(liveCtx); err != nil {
				s.logQuiet(liveCtx, "collecting unused share volumes", "err", err)
				return
			}
			// Here and not in `gc`, whose query session keeps no record.
			s.pruneShareRecord(liveCtx, live)
		})

		s.log().Info("connected to " + s.opts.Config.User + "@" + s.opts.Config.Host)
	}
	return live, nil
}

// dial reaches the workspace as cfg's account. A machine workspace is held
// and located first, every time (ADR 0026); hold keeps it running and is nil
// for any other, and the caller closes it after the client.
func dial(ctx context.Context, cfg config.Config, signer ssh.Signer, hostKey ssh.HostKeyCallback) (_ *tunnelclient.Client, hold io.Closer, err error) {
	transport, err := cfg.Transport()
	if err != nil {
		return nil, nil, err
	}

	host := transport.Host
	if m := cfg.Machine; m != nil {
		// Held first: a WSL machine with no session in it shuts down under a
		// working connection.
		if hold, err = machine.Hold(ctx, m.Backend, m.Name); err != nil {
			return nil, nil, err
		}
		defer func() {
			if err != nil {
				_ = hold.Close()
			}
		}()
		// Its address changes at boot.
		if host, err = machine.Locate(ctx, m.Backend, m.Name, transport.Port); err != nil {
			return nil, nil, err
		}
	}

	dialer, err := dialerFor(transport, cfg)
	if err != nil {
		return nil, nil, err
	}
	client, err := tunnelclient.Dial(ctx, tunnelclient.Config{
		Host:    host,
		Port:    transport.Port,
		User:    cfg.User,
		Signer:  signer,
		HostKey: hostKey,
		Dial:    dialer,
	})
	if err != nil {
		if hint := enrolmentHint(err, cfg.User, signer); hint != "" {
			return nil, nil, fmt.Errorf("%w%s", err, hint)
		}
		return nil, nil, err
	}
	return client, hold, nil
}

// announceRun names this process's run to the workspace (ADR 0050), before
// anything that depends on which client asks.
func (s *Session) announceRun(ctx context.Context, client *tunnelclient.Client) error {
	ctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()

	ok, reply, err := client.SendRequest(ctx, workspace.RunRequest, []byte(s.runID))
	if err != nil {
		return fmt.Errorf("naming this run to the workspace: %w", err)
	}
	// A refusal with no reason is an agent that predates runs, and this client
	// is then a machine, as it always was.
	if !ok && len(reply) > 0 {
		return fmt.Errorf("the workspace refused this run: %s", reply)
	}
	return nil
}

// clientIDFor is the id this client goes by: the one the workspace derived for
// an ephemeral run, which needs the run id, or else the key's (ADR 0029).
func clientIDFor(info workspace.Info, publicKeyWire []byte) string {
	if info.Client != "" {
		return info.Client
	}
	return workspace.ClientID(publicKeyWire)
}

// hostKeyRule is the host key a workspace must offer. A machine this program
// built must offer the key it was built with, at whatever address it has
// (ADR 0026); anything else goes through known_hosts.
func hostKeyRule(cfg config.Config) (ssh.HostKeyCallback, error) {
	if m := cfg.Machine; m != nil && m.HostKey != "" {
		want, _, _, _, err := ssh.ParseAuthorizedKey([]byte(m.HostKey))
		if err != nil {
			return nil, fmt.Errorf("the host key recorded for %q cannot be read: %w", cfg.Name, err)
		}
		return func(_ string, _ net.Addr, offered ssh.PublicKey) error {
			if bytes.Equal(offered.Marshal(), want.Marshal()) {
				return nil
			}
			return fmt.Errorf("the %s machine %q answered with a host key it was not built with (%s)\n"+
				"  fix: stop whatever else listens on port %d, or `remote machine rebuild %s`",
				m.Backend, cfg.Name, ssh.FingerprintSHA256(offered), cfg.Port, cfg.Name)
		}, nil
	}
	known, err := keys.NewKnownHosts(config.KnownHostsPath())
	if err != nil {
		return nil, err
	}
	return known.Callback(), nil
}

// dialerFor returns the WebSocket dialer (ADR 0034), or nil to dial TCP.
func dialerFor(t config.Transport, cfg config.Config) (func(context.Context) (net.Conn, error), error) {
	if !t.WebSocket() {
		return nil, nil
	}
	return tunnelclient.WebSocketDialer(tunnelclient.WebSocketOptions{
		URL:      t.URL,
		Addr:     net.JoinHostPort(t.Host, strconv.Itoa(t.Port)),
		CAFile:   cfg.CAFile,
		Insecure: cfg.Insecure,
	})
}

// ensureCacheChan opens the cache channel once per connection, keeping the
// error. Lazily, not at connect: a session with only write=through mounts must
// never ask, so it stays silent against an older workspace.
func (s *Session) ensureCacheChan(ctx context.Context, l *liveConn) (*cacheChannel, error) {
	l.cacheOnce.Do(func() {
		l.cacheChan, l.cacheErr = openCache(ctx, l.ssh)
	})
	return l.cacheChan, l.cacheErr
}

func (s *Session) shareCacheFor(ctx context.Context, l *liveConn) (rewrite.Cache, error) {
	c, err := s.ensureCacheChan(ctx, l)
	if err != nil {
		return nil, cacheRefusal(err, l.info.Agent)
	}
	return shareCache{cacheChannel: c, session: s}, nil
}

// skew is the workspace's clock minus this machine's; zero from an agent that
// reports no clock.
func (s *Session) skew() time.Duration {
	live, ok := s.gate.currentLive()
	if !ok || live == nil || live.info.Now == 0 {
		return 0
	}
	return live.clockSkew
}

const shareReconcileInterval = 30 * time.Second

// syncWatch points the watcher at whatever the registry exports now.
func (s *Session) syncWatch() {
	if s.watch != nil {
		s.watch.Sync(sharesOf(s.registry))
	}
}

// logQuiet reports an error unless its work was cancelled: tearing down the
// connection fails every goroutine on it, and printing that made a clean exit
// look like a crash.
func (s *Session) logQuiet(ctx context.Context, msg string, args ...any) {
	if ctx.Err() != nil || s.ctx.Err() != nil {
		return
	}
	s.log().Warn(msg, args...)
}

const refusalReasonTimeout = 10 * time.Second

// refusalReason asks the workspace why the reverse forward was refused, since
// the refusal carries no reason (RFC 4254). Asked afresh: the daemon may have
// failed since live.info was read.
func (s *Session) refusalReason(live *liveConn) string {
	ctx, cancel := context.WithTimeout(s.ctx, refusalReasonTimeout)
	defer cancel()

	if info, err := readInfo(ctx, live.ssh); err == nil && info.Docker == workspace.DockerUnavailable {
		return "\n  your docker daemon on the workspace is not running, and the tunnel is bound inside it" +
			"\n  fix: try again in a moment; if it persists, the workspace operator can see why with " +
			"`remote-dockerd daemons ls`"
	}
	return "\n  another session for this account may still hold that port" +
		"\n  fix: close it, or wait about a minute for the workspace to notice it is gone"
}

func (s *Session) startNFS(live *liveConn) error {
	addr := net.JoinHostPort("127.0.0.1", fmt.Sprint(live.info.NFSPort))

	l, err := live.ssh.Listen(addr)
	if err != nil {
		return fmt.Errorf("reserving %s on the workspace: %w%s", addr, err, s.refusalReason(live))
	}
	live.nfsTunnel = l
	// An ephemeral run with no port yet asked for 0 and was given one (ADR 0050).
	if a, ok := l.Addr().(*net.TCPAddr); ok && live.info.NFSPort == 0 {
		live.info.NFSPort = a.Port
	}

	// The session's server, so its handles survive the reconnect (Session.nfs).
	live.wg.Go(func() {
		if err := s.nfs.Serve(l); err != nil && !errors.Is(err, net.ErrClosed) {
			s.logQuiet(s.ctx, "the nfs server stopped", "err", err)
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
			return c.Labels[workspace.OwnerLabel] == live.info.User
		},

		LocalPorts: func(c ports.Container, p ports.Published) []int {
			return localPortsFor(c, p, s.clientID)
		},
	}
	live.wg.Go(func() { s.watchPorts(ctx, live) })
}

// watchPorts reconciles on container events and on a timer, since the event
// stream can drop.
func (s *Session) watchPorts(ctx context.Context, live *liveConn) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	reconcile := func() {
		if err := live.ports.Reconcile(ctx); err != nil {
			s.logQuiet(ctx, "reconciling ports", "err", err)
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

// nconnect reads REMOTE_DOCKER_NFS_NCONNECT once, so a bad value is warned
// about once rather than on every reconnect.
func (s *Session) nconnect() int {
	nconnectOnce.Do(func() {
		n, err := rewrite.NConnect()
		if err != nil {
			s.log().Warn("ignoring " + err.Error())
			return
		}
		nconnectValue = n
	})
	return nconnectValue
}

var (
	nconnectOnce  sync.Once
	nconnectValue int
)

// localPortFree reports whether this machine can open port for a container
// about to be created: not a forward this session holds, and not bound by
// anything else.
func localPortFree(live *liveConn, port int) error {
	if live.ports != nil && live.ports.Forwarding(port) {
		return fmt.Errorf("this session already forwards it")
	}

	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return fmt.Errorf("something on this machine is listening there")
	}
	return l.Close()
}

// localPortsFor is the ports to open here for one published port, or nil to
// use the published one. Only on the machine that asked (ADR 0008, ADR 0029);
// several when one container port was published more than once.
func localPortsFor(c ports.Container, p ports.Published, clientID string) []int {
	if c.Labels[workspace.ClientLabel] != clientID {
		return nil
	}
	return workspace.ParseRequestedPorts(c.Labels[workspace.PortsLabel])[workspace.ContainerPort(p.PrivatePort, p.Type)]
}
