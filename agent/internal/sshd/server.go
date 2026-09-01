// Package sshd is the workspace's SSH server.
//
// It replaces OpenSSH in the workspace image, along with the sudoers file and
// the mount helpers that existed only to work around being a shell (ADR 0010).
// Authentication, port ownership and the workspace's own RPCs all happen in
// this process, so policy is code rather than generated configuration.
package sshd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"

	gssh "github.com/gliderlabs/ssh"
	"golang.org/x/crypto/ssh"

	"github.com/lhns/remote-docker/agent/internal/daemons"
	"github.com/lhns/remote-docker/agent/internal/unions"
	"github.com/lhns/remote-docker/core-agent/accounts"
	"github.com/lhns/remote-docker/core-agent/tunnelserver"
	"github.com/lhns/remote-docker/core/logx"
	"github.com/lhns/remote-docker/core/tunnel"
	"github.com/lhns/remote-docker/core/workspace"
)

// Config configures the server.
type Config struct {
	Addr     string
	HostKeys []ssh.Signer

	Accounts *accounts.Store
	Mapping  workspace.Mapping

	// Daemons resolves an account to the daemon that serves it: its socket, its
	// DOCKER_HOST, its network namespace and its filesystem root.
	//
	// ONE field, with the implementation chosen once by the caller:
	// daemons.Shared for ADR 0012's single daemon, a *daemons.Manager for ADR
	// 0019's one per account. Never reintroduce a nil check at the use sites --
	// that is the shape a routing mistake hides in, because sending a session
	// to the wrong daemon does not fail, it succeeds against somebody else's
	// containers.
	Daemons daemons.Targets

	// Ports decides which port serves which of an account's machines, and
	// remembers it. Nil falls back to the uid-derived port for everybody,
	// which is one machine per account and how this worked before ADR 0029.
	Ports *accounts.Ports

	// Version is the agent's build, reported in workspace-info so a client can
	// see which workspace agent it is talking to.
	Version string

	// Unions mounts a delegated share as a cache over the live export
	// (ADR 0044). Nil means this workspace does not serve the mode at all,
	// which workspace-info reports as an empty capability and the client
	// refuses by name.
	Unions *unions.Manager

	// DaemonPaths are the paths a bind may name because the workspace put them
	// in the daemon's own filesystem, derived from WORKSPACE_DIND_MOUNTS and
	// reported in workspace-info (ADR 0041). The client leaves such a bind
	// alone instead of exporting it. Empty is the old behaviour.
	DaemonPaths []string

	Log *slog.Logger
}

// Server serves SSH for the workspace.
type Server struct {
	cfg     Config
	forward *ForwardPolicy
	ssh     *gssh.Server

	// tcpip is the forwarding protocol, in the shared module, answering to the
	// policies below. See core-agent/tunnelserver.
	tcpip tunnelserver.Forwards

	mu     sync.Mutex
	closed bool
}

// errNoAccount is returned when a forward arrives on a connection with no
// authenticated account, which cannot happen and must not be treated as
// permission if it does.
var errNoAccount = errors.New("sshd: no authenticated account on this connection")

// sessionAccount adapts an account to the forward policy's view of one.
type sessionAccount struct {
	name string
	uid  int

	// client names the MACHINE this session came from, derived from the key
	// that just authenticated rather than from anything the client sent. Two
	// of somebody's machines share an account and a daemon; only this tells
	// their exports and volumes apart.
	client string
}

func (s sessionAccount) Name() string   { return s.name }
func (s sessionAccount) UID() int       { return s.uid }
func (s sessionAccount) Client() string { return s.client }

// contextKey is the type under which the authenticated account is stored.
type contextKey struct{}

// New builds a server.
func New(cfg Config) (*Server, error) {
	if cfg.Accounts == nil {
		return nil, fmt.Errorf("sshd: an account store is required")
	}
	// A missing resolver is the shared daemon at its usual socket, so a caller
	// that says nothing gets ADR 0012's arrangement rather than a nil
	// dereference on the first session.
	if cfg.Daemons == nil {
		cfg.Daemons = daemons.Shared("")
	}

	// A nil allocator is not an error: with nowhere to remember, every client
	// of an account gets the uid-derived port, which is what every version
	// before ADR 0029 did and is right for the one-machine case. Defaulted
	// here so that nothing downstream has to ask whether it has one.
	if cfg.Ports == nil {
		cfg.Ports = &accounts.Ports{Mapping: cfg.Mapping}
	}

	s := &Server{cfg: cfg, forward: NewForwardPolicy(cfg.Mapping)}
	s.forward.Ports = cfg.Ports
	s.tcpip = tunnelserver.Forwards{
		Reverse: reversePolicy{s},
		Local:   localPolicy{s},
		Log:     cfg.Log,
	}

	s.ssh = &gssh.Server{
		Addr:             cfg.Addr,
		PublicKeyHandler: s.authenticate,

		// A client that vanishes without saying so must still end its
		// connection here, because that is what releases its reverse-tunnel
		// port. See armDeadPeerDetection.
		ConnCallback: func(_ gssh.Context, conn net.Conn) net.Conn {
			armDeadPeerDetection(conn)
			return conn
		},

		// Reverse forwarding carries the client's NFS export in; local
		// forwarding lets the client reach published container ports. Both are
		// needed, and both are constrained. See ForwardPolicy.
		//
		// The callbacks are deliberately NOT set. gliderlabs invokes them from
		// the handlers we replaced, so setting them here would leave the
		// permission check in two places, and reversePolicy.Allow is not a
		// predicate: it binds the port and arms the release. Called twice, the
		// second call refuses its own reservation.

		// The machinery is core-agent/tunnelserver's rather than gliderlabs', because
		// both of theirs hardcode the namespace they listen and dial in. The
		// decisions it asks for are in forward_tcpip.go.
		RequestHandlers: map[string]gssh.RequestHandler{
			"tcpip-forward":        s.tcpip.HandleRequest,
			"cancel-tcpip-forward": s.tcpip.HandleRequest,
		},
		ChannelHandlers: map[string]gssh.ChannelHandler{
			"session":      gssh.DefaultSessionHandler,
			"direct-tcpip": s.tcpip.HandleChannel,

			// Datagrams to a published UDP port. A client whose workspace
			// predates this asks for a channel type the server does not know
			// and is refused, which is the whole version check (ADR 0038).
			tunnel.UDPChannelType: s.tcpip.HandleUDPChannel,
		},

		Handler: s.handleSession,
	}

	for _, key := range cfg.HostKeys {
		s.ssh.AddHostKey(key)
	}
	return s, nil
}

// authenticate accepts a key only for the account it is enrolled against.
func (s *Server) authenticate(ctx gssh.Context, key gssh.PublicKey) bool {
	name := ctx.User()

	account, ok := s.cfg.Accounts.Lookup(name)
	if !ok {
		s.log().Warn("refused a connection: no such account", "account", name, "from", ctx.RemoteAddr())
		return false
	}
	if !account.Authorized(key) {
		// Covers a revoked account too: revocation empties the key list, so
		// the account survives while its access does not.
		s.log().Warn("refused a connection: the key is not enrolled", "account", name, "from", ctx.RemoteAddr())
		return false
	}

	ctx.SetValue(contextKey{}, sessionAccount{
		name: account.Name,
		uid:  account.UID,
		// From the key that just passed, which is what makes the id
		// authenticated rather than asserted.
		client: workspace.ClientID(key.Marshal()),
	})

	// Start this account's daemon now, in the background, so its boot hides
	// behind the round trips that follow: workspace-info, then the reverse
	// forward. A cold dind takes seconds; without this the client's first
	// docker command pays for all of them, looking like a hang rather than a
	// start.
	s.cfg.Daemons.Warm(account.Name)
	return true
}

// accountFor returns the authenticated account for a connection.
func accountFor(ctx gssh.Context) (sessionAccount, bool) {
	account, ok := ctx.Value(contextKey{}).(sessionAccount)
	return account, ok
}

// Serve accepts connections until the server is closed.
func (s *Server) Serve(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("sshd: listening on %s: %w", s.cfg.Addr, err)
	}

	go func() {
		<-ctx.Done()
		_ = s.Close()
	}()

	s.log().Info("listening on " + s.cfg.Addr)
	if err := s.ssh.Serve(listener); err != nil && !isClosed(err) {
		return fmt.Errorf("sshd: serving: %w", err)
	}
	return nil
}

// ServeListener accepts connections from somewhere other than the TCP port.
//
// The same server, the same authentication and the same forwarding policy: only
// what carries the bytes differs, and nothing above the transport is told which
// it got. Used for the WebSocket listener, which exists so a workspace can be
// reached through a reverse proxy.
//
// Runs until the listener or the server closes, so callers start it in a
// goroutine of its own alongside Serve.
func (s *Server) ServeListener(l net.Listener) error {
	if err := s.ssh.Serve(l); err != nil && !isClosed(err) {
		return fmt.Errorf("sshd: serving %s: %w", l.Addr(), err)
	}
	return nil
}

// Close stops the server.
func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.ssh.Close()
}

// Addr reports the address actually bound, which matters when the configured
// port is 0.
func (s *Server) Addr() string { return s.cfg.Addr }

func isClosed(err error) bool {
	return err == gssh.ErrServerClosed || err == net.ErrClosed
}

// log is the server's logger, or silence. See logx.Or.
func (s *Server) log() *slog.Logger {
	return logx.Or(s.cfg.Log)
}
