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

	"github.com/lhns/remote-docker/internal/logx"
	"github.com/lhns/remote-docker/internal/server/accounts"
	"github.com/lhns/remote-docker/internal/server/daemons"
	"github.com/lhns/remote-docker/pkg/workspace"
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
	// One field, with the implementation chosen once by the caller --
	// daemons.Shared for ADR 0012's single daemon, a *daemons.Manager for ADR
	// 0019's one per account. It used to be two fields and a nil check repeated
	// at nine call sites, which is precisely the shape a routing mistake hides
	// in: sending a session to the wrong daemon does not fail, it succeeds
	// against somebody else's containers.
	Daemons daemons.Targets

	// Version is the agent's build, reported in workspace-info so a client can
	// see which workspace agent it is talking to.
	Version string

	Log *slog.Logger
}

// Server serves SSH for the workspace.
type Server struct {
	cfg     Config
	forward *ForwardPolicy
	ssh     *gssh.Server

	tcpip forwardedTCP

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
}

func (s sessionAccount) Name() string { return s.name }
func (s sessionAccount) UID() int     { return s.uid }

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

	s := &Server{cfg: cfg, forward: NewForwardPolicy(cfg.Mapping)}

	s.ssh = &gssh.Server{
		Addr:             cfg.Addr,
		PublicKeyHandler: s.authenticate,

		// Reverse forwarding carries the client's NFS export in; local
		// forwarding lets the client reach published container ports. Both are
		// needed, and both are constrained -- see ForwardPolicy.
		//
		// The callbacks are deliberately NOT set. gliderlabs invokes them from
		// the handlers we replaced, so setting them here would leave the
		// permission check in two places -- and allowReverseForward is not a
		// predicate: it binds the port and arms the release. Called twice, the
		// second call refuses its own reservation.

		// Ours rather than gliderlabs', because both of theirs hardcode the
		// namespace they listen and dial in -- see forward_tcpip.go.
		RequestHandlers: map[string]gssh.RequestHandler{
			"tcpip-forward":        s.handleForwardRequest,
			"cancel-tcpip-forward": s.handleForwardRequest,
		},
		ChannelHandlers: map[string]gssh.ChannelHandler{
			"session":      gssh.DefaultSessionHandler,
			"direct-tcpip": s.directTCPIP,
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

	ctx.SetValue(contextKey{}, sessionAccount{name: account.Name, uid: account.UID})

	// Start this account's daemon now, in the background, so its boot hides
	// behind the round trips that follow -- workspace-info, then the reverse
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

// allowReverseForward gates `ssh -R`, which is how the client's NFS export
// reaches the workspace. This is where ADR 0010's claim is enforced.
func (s *Server) allowReverseForward(ctx gssh.Context, host string, port uint32) bool {
	account, ok := accountFor(ctx)
	if !ok {
		return false
	}

	allowed, why := s.forward.Allow(account, host, port)
	if !allowed {
		s.log().Warn("refused a reverse forward", "host", host, "port", port, "account", account.Name(), "why", why)
		return false
	}
	if !s.forward.Bind(account, host, port) {
		s.log().Warn("refused a reverse forward: the port is already held",
			"host", host, "port", port, "account", account.Name())
		return false
	}

	// Released when the connection ends, so a dropped client does not keep its
	// port reserved forever.
	go func() {
		<-ctx.Done()
		s.forward.Release(account, host, port)
	}()

	s.log().Info("forwarding", "account", account.Name(), "host", host, "port", port)
	return true
}

// allowLocalForward gates `ssh -L`, which the client uses to reach published
// container ports.
//
// Unconstrained by port, deliberately: the ports a container publishes are not
// knowable in advance, and everything reachable this way is inside the
// workspace, which the account can already reach with a shell. Restricting to
// loopback still matters -- it stops the workspace being used to reach the
// wider network it happens to sit on.
func (s *Server) allowLocalForward(ctx gssh.Context, host string, port uint32) bool {
	if _, ok := accountFor(ctx); !ok {
		return false
	}
	if !isLoopback(host) {
		s.log().Warn("refused a local forward: only loopback may be reached", "host", host, "port", port)
		return false
	}
	return true
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

// log is the server's logger, or silence. A nil *slog.Logger panics on use
// rather than doing nothing, so the zero value needs an answer.
func (s *Server) log() *slog.Logger {
	if s.cfg.Log == nil {
		return logx.Discard()
	}
	return s.cfg.Log
}
