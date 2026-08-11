// Bringing a connection up, and everything that lives only while it is up.
//
// The ORDER in here is load-bearing and is the reason it is one file: the NFS
// export has to be reachable before any container can mount a volume backed by
// it, and the things only a hosting session starts (ports, notify, the volume
// collector) must not be started by one that merely asks a question.

package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"

	"github.com/lhns/remote-docker/client/internal/config"
	"github.com/lhns/remote-docker/client/internal/machine"
	"github.com/lhns/remote-docker/client/internal/nfsserve"
	"github.com/lhns/remote-docker/client/internal/ports"
	"github.com/lhns/remote-docker/client/internal/proxy"
	"github.com/lhns/remote-docker/client/internal/rewrite"
	"github.com/lhns/remote-docker/client/internal/sshx"

	"github.com/lhns/remote-docker/pkg/workspace"
)

// connect brings up everything that needs the workspace. The order matters:
// the NFS export has to be reachable before any container can mount a volume
// backed by it.
func (s *Session) connect(ctx context.Context) (*liveConn, error) {
	key, err := sshx.LoadOrCreateKey(config.KeyPath(), config.KeyComment())
	if err != nil {
		return nil, err
	}

	// This machine's name for itself, and the workspace derives the same one
	// from the key it authenticates rather than from anything sent to it. See
	// workspace.ClientID: the account is the identity, the client is the
	// machine, and only the second can tell one of somebody's computers from
	// another when both use one account.
	s.clientID = workspace.ClientID(key.Signer.PublicKey().Marshal())

	known, err := sshx.NewKnownHosts(config.KnownHostsPath())
	if err != nil {
		return nil, err
	}

	// THE one place a machine is located.
	//
	// A workspace on another host is simply there; a machine on this one has to
	// be running before it can answer, and its address is given to it at boot,
	// so a stored one goes stale the moment it restarts. Locate does both, and
	// it is here rather than in the commands because every path to a session
	// comes through this function -- a check at `machine create` would be right
	// for the first connection and wrong for every one after a reboot.
	host := s.opts.Config.Host
	var hold io.Closer
	if m := s.opts.Config.Machine; m != nil {
		// Held first, for as long as this connection lives. A machine with
		// nobody in it shuts down, and an open TCP connection is not somebody:
		// WSL counts its own sessions, so without this the machine can go away
		// underneath a working session.
		if hold, err = machine.Hold(ctx, m.Backend, m.Name); err != nil {
			return nil, err
		}
		host, err = machine.Locate(ctx, m.Backend, m.Name)
		if err != nil {
			_ = hold.Close()
			return nil, err
		}
	}

	client, err := sshx.Dial(ctx, sshx.Config{
		Host:       host,
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

	live := &liveConn{ssh: client, info: info, machine: hold}
	live.api = &proxy.APIClient{Dialer: &proxy.SSHDialer{Client: client}}
	// One guard for this connection, shared by the two things that disagree
	// about a volume's lifetime. See rewrite.Guard.
	live.guard = &rewrite.Guard{Exported: s.exportsVolume}
	live.rewriter = &rewrite.Rewriter{
		Shares:  shareRegistrar{registry: s.registry, shares: s.shares, changed: s.sharesChanged},
		Volumes: live.api,
		NFSPort: info.NFSPort,
		Owner:   info.User,
		Client:  s.clientID,
		Guard:   live.guard,
	}
	if s.opts.Role.hosting() {
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
	// A Query session (`status`, `gc`) only asks the workspace a
	// question and then closes. Starting them there meant a status command
	// began two background round trips and immediately tore the connection out
	// from under them, so it printed its table followed by two errors about
	// work the user never asked for.
	if s.opts.Role.hosting() {
		s.startPorts(liveCtx, live)
		s.startNotify(live)

		live.wg.Go(func() {
			if _, err := s.collector(live).Collect(liveCtx); err != nil {
				s.logQuiet(liveCtx, "collecting unused share volumes", "err", err)
			}
		})
	}

	s.progressf("connected to " + s.opts.Config.User + "@" + s.opts.Config.Host)
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

// progressf reports routine progress, which most commands do not want.
func (s *Session) progressf(msg string, args ...any) {
	if s.opts.Role.hosting() {
		s.log().Info(msg, args...)
	}
}

// portsLogger is the logger the port manager gets: the real one when progress
// is wanted, and nil otherwise, because "forwarding ..." arriving in the
// middle of a container's output is exactly the pollution this avoids.
func (s *Session) portsLogger() *slog.Logger {
	if s.opts.Role.hosting() {
		return s.opts.Log
	}
	return nil
}

// logQuiet reports an error unless the context that owns the work has already
// been cancelled.
//
// Everything here talks over one SSH connection, so tearing that connection
// down makes every goroutine still using it fail at once, with EOF, or
// "unexpected packet in response to channel open", or a half-read stream.
// Those are descriptions of shutdown, not of anything wrong, and printing them
// after the user pressed Ctrl-C or after a one-shot command finished is how a
// clean exit came to look like a crash.
func (s *Session) logQuiet(ctx context.Context, msg string, args ...any) {
	if ctx.Err() != nil || s.ctx.Err() != nil {
		return
	}
	s.log().Warn(msg, args...)
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
			s.logQuiet(s.ctx, "the nfs server stopped", "err", err)
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
