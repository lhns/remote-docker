// Package sshd is the workspace's SSH server. Authentication, port ownership
// and the workspace's own RPCs all happen in this process, so policy is code
// rather than generated configuration (ADR 0010).
package sshd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"

	gssh "github.com/gliderlabs/ssh"
	"golang.org/x/crypto/ssh"

	"github.com/lhns/remote-docker/agent/internal/daemons"
	"github.com/lhns/remote-docker/agent/internal/ephemeral"
	"github.com/lhns/remote-docker/agent/internal/metrics"
	"github.com/lhns/remote-docker/agent/internal/unions"
	"github.com/lhns/remote-docker/core-agent/accounts"
	"github.com/lhns/remote-docker/core-agent/tokens"
	"github.com/lhns/remote-docker/core-agent/tunnelserver"
	"github.com/lhns/remote-docker/core/enrol"
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

	// Daemons resolves an account to the daemon that serves it. ONE field, with
	// the implementation chosen once by the caller: daemons.Shared (ADR 0012)
	// or a *daemons.Manager (ADR 0019). Never reintroduce a nil check at a use
	// site; see daemons.Targets.
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

	// Ephemeral names the accounts whose every client run is a client of its
	// own, from WORKSPACE_EPHEMERAL_ACCOUNTS (ADR 0050).
	Ephemeral map[string]bool

	// Runs holds those accounts' runs: their grace period, their limit and
	// their ports. Nil is a registry with the defaults and no record.
	Runs *ephemeral.Registry

	// Tokens are the enrolment tokens a `+token:` login may redeem (ADR
	// 0051), and Limiter bounds failed redemptions. Nil Tokens refuses every
	// token login; nil Limiter is tokens.NewLimiter.
	Tokens  *tokens.Store
	Limiter *tokens.Limiter

	// Admins are the accounts named in WORKSPACE_ADMINS, folded, which may
	// manage every account (ADR 0053). Their names are never given to an
	// unbound token.
	Admins map[string]bool

	Metrics Metrics

	Log *slog.Logger
}

// Metrics are the counters the server increments (ADR 0054). A nil one counts
// nothing.
type Metrics struct {
	// Redemptions counts token redemptions by outcome: RedeemOK,
	// RedeemPending, RedeemRefused or RedeemFailed.
	Redemptions *metrics.Counter

	// LimiterRejections counts redemptions the global limiter turned away.
	LimiterRejections *metrics.Counter

	// RunsRefused counts refused run requests by reason. The registry counts
	// its own refusals into the same counter; this server counts
	// RefusedDuplicate and RefusedMalformed.
	RunsRefused *metrics.Counter
}

// Outcomes of a redemption, and reasons a run request is refused here, as the
// counters label them.
const (
	RedeemOK      = "ok"
	RedeemPending = "pending"
	RedeemRefused = "refused"
	RedeemFailed  = "failed"

	RefusedDuplicate = "duplicate"
	RefusedMalformed = "malformed"
)

// Server serves SSH for the workspace.
type Server struct {
	cfg     Config
	forward *ForwardPolicy
	ssh     *gssh.Server

	// tcpip is the forwarding protocol, in the shared module, answering to the
	// policies below. See core-agent/tunnelserver.
	tcpip tunnelserver.Forwards

	// query stands in for the docker CLI in tests, which have no daemon and
	// need to see which host a question went to. Nil is the real CLI.
	query func(ctx context.Context, host string, args ...string) (string, error)

	conns   conns // see revoke.go
	limiter *tokens.Limiter

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
	//
	// For an ephemeral account it names the RUN, derived from key, and is
	// empty until the run request arrives (ADR 0050).
	client    string
	ephemeral bool
	key       []byte

	// fingerprint is the key this connection authenticated with.
	fingerprint string
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
	// of an account gets the uid-derived port, which is right for the
	// one-machine case (ADR 0029). Defaulted here so nothing downstream has to
	// ask whether it has one.
	if cfg.Ports == nil {
		cfg.Ports = &accounts.Ports{Mapping: cfg.Mapping}
	}
	if cfg.Runs == nil {
		cfg.Runs = &ephemeral.Registry{Ports: cfg.Ports, Log: cfg.Log}
	}

	s := &Server{cfg: cfg, forward: NewForwardPolicy(cfg.Mapping), limiter: cfg.Limiter}
	if s.limiter == nil {
		s.limiter = tokens.NewLimiter()
	}
	s.forward.Ports = cfg.Ports
	s.tcpip = tunnelserver.Forwards{
		Reverse: reversePolicy{s},
		Local:   localPolicy{s},
		Log:     cfg.Log,
	}

	cfg.Accounts.Subscribe(s.sweep)

	s.ssh = &gssh.Server{
		Addr:             cfg.Addr,
		PublicKeyHandler: s.authenticate,
		BannerHandler:    banner,

		// authenticate also answers a key the client only offers; this runs
		// once a key has signed, so only the key that logged in is recorded.
		ServerConfigCallback: func(ctx gssh.Context) *ssh.ServerConfig {
			return &ssh.ServerConfig{
				VerifiedPublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey, perms *ssh.Permissions, _ string) (*ssh.Permissions, error) {
					return perms, s.loggedIn(ctx, key)
				},
			}
		},

		// A client that vanishes without saying so must still end its
		// connection here, because that is what releases its reverse-tunnel
		// port. See armDeadPeerDetection.
		ConnCallback: func(ctx gssh.Context, conn net.Conn) net.Conn {
			armDeadPeerDetection(conn)
			s.conns.track(ctx, conn)
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
			workspace.RunRequest:   s.handleRun,
		},
		ChannelHandlers: map[string]gssh.ChannelHandler{
			"session":      gssh.DefaultSessionHandler,
			"direct-tcpip": s.tcpip.HandleChannel,

			// Datagrams to a published UDP port. A client whose workspace
			// predates this asks for a channel type the server does not know
			// and is refused, which is the whole version check (ADR 0038).
			tunnel.UDPChannelType: s.tcpip.HandleUDPChannel,
		},

		Handler: s.route,
	}

	for _, key := range cfg.HostKeys {
		s.ssh.AddHostKey(key)
	}
	return s, nil
}

// authenticate accepts a key only for the account it is enrolled against. The
// login name is folded as the key file's name was, so Alice reaches alice.
// It also answers a key the client only offers, so it records nothing beyond
// this connection's context: loggedIn does that once the key has signed.
func (s *Server) authenticate(ctx gssh.Context, key gssh.PublicKey) bool {
	// Before folding, which would turn the prefix into part of a name.
	if id, ok := strings.CutPrefix(ctx.User(), enrol.LoginPrefix); ok {
		return s.authenticateToken(ctx, id, key)
	}

	name, err := workspace.AccountName(ctx.User())
	if err != nil {
		s.log().Warn("refused a connection: no such account", "login", ctx.User(), "from", ctx.RemoteAddr())
		return false
	}

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

	session := sessionAccount{name: account.Name, uid: account.UID, fingerprint: ssh.FingerprintSHA256(key)}
	if s.cfg.Ephemeral[account.Name] {
		session.ephemeral = true
		session.key = key.Marshal()
	} else {
		// From the key that just passed, which is what makes the id
		// authenticated rather than asserted.
		session.client = workspace.ClientID(key.Marshal())
	}
	ctx.SetValue(contextKey{}, session)
	ctx.SetValue(redeemerKey{}, nil)
	return true
}

// loggedIn runs once key has signed, which x/crypto does only after
// authenticate accepted that same key last. An error refuses the login.
func (s *Server) loggedIn(ctx gssh.Context, key ssh.PublicKey) error {
	if r, ok := redeemerFor(ctx); ok {
		if !bytes.Equal(r.key.Marshal(), key.Marshal()) {
			return errors.New("sshd: a token login signed with a key it did not offer")
		}
		return nil
	}
	account, ok := accountFor(ctx)
	if !ok || account.fingerprint != ssh.FingerprintSHA256(key) {
		return errors.New("sshd: a login signed with a key it did not offer")
	}
	if !s.authenticated(ctx, account.name, key) {
		s.log().Warn("refused a connection: the key was revoked during the handshake", "account", account.name, "from", ctx.RemoteAddr())
		return errors.New("sshd: the key was revoked during the handshake")
	}

	// Start this account's daemon now, in the background, so its boot hides
	// behind the round trips that follow: workspace-info, then the reverse
	// forward. A cold dind takes seconds; without this the client's first
	// docker command pays for all of them, looking like a hang rather than a
	// start.
	s.cfg.Daemons.Warm(account.name)
	return nil
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

	// The listener too: a Close that lands before ssh.Serve registers it
	// closes nothing, and Serve then blocks in Accept for good.
	go func() {
		<-ctx.Done()
		_ = s.Close()
		_ = listener.Close()
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

func isClosed(err error) bool {
	return err == gssh.ErrServerClosed || err == net.ErrClosed
}

// log is the server's logger, or silence. See logx.Or.
func (s *Server) log() *slog.Logger {
	return logx.Or(s.cfg.Log)
}
