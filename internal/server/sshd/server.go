// Package sshd is the workspace's SSH server.
//
// It replaces OpenSSH in the workspace image, along with the sudoers file and
// the mount helpers that existed only to work around being a shell (ADR 0010).
// Authentication, port ownership and the workspace's own RPCs all happen in
// this process, so policy is code rather than generated configuration.
package sshd

import (
	"context"
	"fmt"
	"net"
	"sync"

	gssh "github.com/gliderlabs/ssh"
	"golang.org/x/crypto/ssh"

	"github.com/lhns/remote-docker/internal/server/accounts"
	"github.com/lhns/remote-docker/pkg/workspace"
)

// Logger reports connections and refusals.
type Logger interface {
	Printf(format string, args ...any)
}

// Config configures the server.
type Config struct {
	Addr     string
	HostKeys []ssh.Signer

	Accounts *accounts.Store
	Mapping  workspace.Mapping

	// DockerSocket is spliced directly to a session asking for
	// `docker system dial-stdio`, so the Docker API needs no CLI in the path.
	DockerSocket string

	Log Logger
}

// Server serves SSH for the workspace.
type Server struct {
	cfg     Config
	forward *ForwardPolicy
	ssh     *gssh.Server

	mu     sync.Mutex
	closed bool
}

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
	if cfg.DockerSocket == "" {
		cfg.DockerSocket = "/var/run/docker.sock"
	}

	s := &Server{cfg: cfg, forward: NewForwardPolicy(cfg.Mapping)}

	forwardHandler := &gssh.ForwardedTCPHandler{}

	s.ssh = &gssh.Server{
		Addr:             cfg.Addr,
		PublicKeyHandler: s.authenticate,

		// Reverse forwarding carries the client's NFS export in; local
		// forwarding lets the client reach published container ports. Both are
		// needed, and both are constrained -- see ForwardPolicy.
		ReversePortForwardingCallback: s.allowReverseForward,
		LocalPortForwardingCallback:   s.allowLocalForward,

		RequestHandlers: map[string]gssh.RequestHandler{
			"tcpip-forward":        forwardHandler.HandleSSHRequest,
			"cancel-tcpip-forward": forwardHandler.HandleSSHRequest,
		},
		ChannelHandlers: map[string]gssh.ChannelHandler{
			"session":      gssh.DefaultSessionHandler,
			"direct-tcpip": gssh.DirectTCPIPHandler,
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
		s.logf("refused %s from %s: no such account", name, ctx.RemoteAddr())
		return false
	}
	if !account.Authorized(key) {
		// Covers a revoked account too: revocation empties the key list, so
		// the account survives while its access does not.
		s.logf("refused %s from %s: key not enrolled", name, ctx.RemoteAddr())
		return false
	}

	ctx.SetValue(contextKey{}, sessionAccount{name: account.Name, uid: account.UID})
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
		s.logf("refused reverse forward %s:%d for %s: %s", host, port, account.Name(), why)
		return false
	}
	if !s.forward.Bind(account, host, port) {
		s.logf("refused reverse forward %s:%d for %s: already held", host, port, account.Name())
		return false
	}

	// Released when the connection ends, so a dropped client does not keep its
	// port reserved forever.
	go func() {
		<-ctx.Done()
		s.forward.Release(account, host, port)
	}()

	s.logf("%s is forwarding %s:%d", account.Name(), host, port)
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
		s.logf("refused local forward to %s:%d: only loopback may be reached", host, port)
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

	s.logf("listening on %s", s.cfg.Addr)
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

func (s *Server) logf(format string, args ...any) {
	if s.cfg.Log != nil {
		s.cfg.Log.Printf(format, args...)
	}
}
