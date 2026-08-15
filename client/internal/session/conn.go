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
	"strconv"
	"time"

	"github.com/lhns/remote-docker/client/internal/config"
	"github.com/lhns/remote-docker/client/internal/machine"
	"github.com/lhns/remote-docker/client/internal/ports"
	"github.com/lhns/remote-docker/client/internal/proxy"
	"github.com/lhns/remote-docker/client/internal/rewrite"
	"github.com/lhns/remote-docker/core-client/keys"
	"github.com/lhns/remote-docker/core-client/tunnelclient"
	"github.com/lhns/remote-docker/core-client/wstunnel"

	"github.com/lhns/remote-docker/core/workspace"
)

// connect brings up everything that needs the workspace. The order matters:
// the NFS export has to be reachable before any container can mount a volume
// backed by it.
func (s *Session) connect(ctx context.Context) (*liveConn, error) {
	key, err := keys.LoadOrCreateKey(config.KeyPath(), config.KeyComment())
	if err != nil {
		return nil, err
	}

	// This machine's name for itself, and the workspace derives the same one
	// from the key it authenticates rather than from anything sent to it. See
	// workspace.ClientID: the account is the identity, the client is the
	// machine, and only the second can tell one of somebody's computers from
	// another when both use one account.
	s.clientID = workspace.ClientID(key.Signer.PublicKey().Marshal())

	known, err := keys.NewKnownHosts(config.KnownHostsPath())
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
	// Whether this workspace is reached over SSH directly or through a reverse
	// proxy (ADR 0034). Worked out here because tunnelclient is handed its
	// connection rather than choosing one (ADR 0030).
	transport, err := s.opts.Config.Transport()
	if err != nil {
		return nil, err
	}

	host := transport.Host
	var hold io.Closer
	if m := s.opts.Config.Machine; m != nil {
		// Held first, for as long as this connection lives. A machine with
		// nobody in it shuts down, and an open TCP connection is not somebody:
		// WSL counts its own sessions, so without this the machine can go away
		// underneath a working session.
		if hold, err = machine.Hold(ctx, m.Backend, m.Name); err != nil {
			return nil, err
		}
		host, err = machine.Locate(ctx, m.Backend, m.Name, transport.Port)
		if err != nil {
			_ = hold.Close()
			return nil, err
		}
	}

	dial, err := dialerFor(transport, s.opts.Config)
	if err != nil {
		if hold != nil {
			_ = hold.Close()
		}
		return nil, err
	}

	client, err := tunnelclient.Dial(ctx, tunnelclient.Config{
		Host:    host,
		Port:    transport.Port,
		User:    s.opts.Config.User,
		Signer:  key.Signer,
		HostKey: known.Callback(),
		Dial:    dial,
	})
	if err != nil {
		// The transport reports that it was refused; only this side knows how
		// somebody is enrolled, so the remedy is attached here.
		if hint := enrolmentHint(err, s.opts.Config.User, key.Signer); hint != "" {
			return nil, fmt.Errorf("%w%s", err, hint)
		}
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
				return
			}
			// Here rather than in Session.Collect, which is the `gc` command
			// and runs on a QUERY session: a query session keeps no record, so
			// pruning there could only ever be a no-op.
			s.pruneShareRecord(liveCtx, live)
		})
	}

	s.progressf("connected to " + s.opts.Config.User + "@" + s.opts.Config.Host)
	return live, nil
}

// dialerFor returns the function that opens the connection, or nil to dial TCP.
//
// Only the transport differs: the SSH handshake, the host-key check and the
// client key are the same for both.
func dialerFor(t config.Transport, cfg config.Config) (func(context.Context) (net.Conn, error), error) {
	if !t.WebSocket() {
		return nil, nil
	}
	return wstunnel.Dialer(wstunnel.Options{
		URL:      t.URL,
		Addr:     net.JoinHostPort(t.Host, strconv.Itoa(t.Port)),
		CAFile:   cfg.CAFile,
		Insecure: cfg.Insecure,
	})
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

// refusalReasonTimeout bounds the one question asked after a refusal. Short:
// the command has already failed and this only decides what to call it.
const refusalReasonTimeout = 10 * time.Second

// refusalReason is why the workspace refused the reverse forward, asked of the
// workspace rather than guessed at.
//
// ssh's tcpip-forward failure carries no reason (RFC 4254 request failure has
// no payload), so this named the likeliest cause whatever had happened, and was
// wrong in the case that produced it: the account's daemon would not start, and
// the forward is bound inside that daemon's namespace.
//
// Asked again rather than read from live.info, which was true when the session
// began: a daemon still booting then may have failed since.
func (s *Session) refusalReason(live *liveConn) string {
	ctx, cancel := context.WithTimeout(s.ctx, refusalReasonTimeout)
	defer cancel()

	if info, err := readInfo(ctx, live.ssh); err == nil && info.Docker == workspace.DockerUnavailable {
		return "\n\tyour docker daemon on the workspace is not running, and the tunnel is bound inside it" +
			"\n\tfix: try again in a moment; if it persists, the workspace operator can see why with " +
			"`remote-dockerd daemons ls`"
	}
	return "\n\tanother session for this account may still hold that port" +
		"\n\tfix: close it, or wait about a minute for the workspace to notice it is gone"
}

func (s *Session) startNFS(live *liveConn) error {
	addr := net.JoinHostPort("127.0.0.1", fmt.Sprint(live.info.NFSPort))

	l, err := live.ssh.Listen(addr)
	if err != nil {
		return fmt.Errorf("reserving %s on the workspace: %w%s", addr, err, s.refusalReason(live))
	}
	live.nfsTunnel = l

	// The SESSION's server, not one built here. Serve returns when this
	// connection's listener closes and the same server takes the next one, so
	// the handle cache spans reconnects and a container that was already
	// running keeps reading. See Session.nfs.
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
