// Package tunnelclient dials the workspace end of the tunnel.
//
// One ssh.Client carries every channel this project needs: the reverse forward
// for the NFS export, a local forward per published container port, the Docker
// API stream, and any interactive session. Multiplexing is inherent to the SSH
// protocol, so there is no connection to share between commands and no
// per-command handshake to pay for -- which shelling out to ssh(1) would need
// OpenSSH's ControlMaster to avoid, and Win32-OpenSSH does not implement it.
//
// It knows nothing about Docker and nothing about who may log in. Both are
// deliberate. Docker is glue and lives in the binaries; auth is policy, so this
// package is handed a signer and a host key callback and never decides which
// key or which trust rule (ADR 0030). The caller building those two values is
// the only place that can also say what to do when they are refused, which is
// why the enrolment hint lives there rather than here.
package tunnelclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/lhns/remote-docker/core/tunnel"
)

// Config describes how to reach a workspace.
type Config struct {
	Host string
	Port int
	User string

	// Signer is this machine's identity. Where the key came from, and whether
	// it was generated on first use, is the caller's business.
	Signer ssh.Signer

	// HostKey decides whether the far side is the workspace it claims to be.
	// Required: there is no default, because every default is either a prompt
	// nobody is there to answer or an acceptance of anything at all.
	HostKey ssh.HostKeyCallback

	// Ciphers, if set, replaces the negotiated cipher list.
	//
	// aes128-gcm is the default for a reason worth keeping: AES-NI makes it
	// several GB/s where ChaCha20 is markedly slower, and every byte of the
	// NFS traffic crosses this connection. There is no double encryption to
	// worry about, because NFS inside the tunnel is plaintext.
	Ciphers []string

	// KeepAlive is how often to probe a connection that is otherwise idle.
	// A tunnel can be dead for a long time without either end noticing, and
	// a dead tunnel means container I/O failing with EIO.
	KeepAlive time.Duration

	Timeout time.Duration
}

// DefaultCiphers preferred, fastest first. See Config.Ciphers.
var DefaultCiphers = []string{
	"aes128-gcm@openssh.com",
	"aes256-gcm@openssh.com",
	"chacha20-poly1305@openssh.com",
}

const (
	defaultKeepAlive = 15 * time.Second
	defaultTimeout   = 30 * time.Second
)

// Addr is the workspace's SSH endpoint.
func (c Config) Addr() string {
	return net.JoinHostPort(c.Host, fmt.Sprint(c.Port))
}

// Client is a live connection to a workspace.
type Client struct {
	ssh  *ssh.Client
	cfg  Config
	once sync.Once
	done chan struct{}
}

// Dial connects and authenticates.
func Dial(ctx context.Context, cfg Config) (*Client, error) {
	// Refused here rather than left to x/crypto, because the shape of the
	// mistake matters: a caller that forgot this has no host key policy at all,
	// and the failure must name that rather than arrive as a handshake error.
	if cfg.HostKey == nil {
		return nil, errors.New("tunnel: Config.HostKey is required")
	}
	if cfg.Signer == nil {
		return nil, errors.New("tunnel: Config.Signer is required")
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.KeepAlive == 0 {
		cfg.KeepAlive = defaultKeepAlive
	}
	if len(cfg.Ciphers) == 0 {
		cfg.Ciphers = DefaultCiphers
	}

	clientCfg := &ssh.ClientConfig{
		User:            cfg.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(cfg.Signer)},
		HostKeyCallback: cfg.HostKey,
		Timeout:         cfg.Timeout,
	}
	clientCfg.Ciphers = cfg.Ciphers

	dialer := &net.Dialer{Timeout: cfg.Timeout}
	conn, err := dialer.DialContext(ctx, "tcp", cfg.Addr())
	if err != nil {
		return nil, fmt.Errorf("tunnel: dialling %s: %w", cfg.Addr(), err)
	}

	sshConn, chans, reqs, err := ssh.NewClientConn(conn, cfg.Addr(), clientCfg)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("tunnel: connecting to %s@%s: %w", cfg.User, cfg.Addr(), err)
	}

	c := &Client{
		ssh:  ssh.NewClient(sshConn, chans, reqs),
		cfg:  cfg,
		done: make(chan struct{}),
	}
	go c.keepAlive()

	// The transport failing is noticed at once rather than at the next probe.
	// Wait returns as soon as the connection is torn down, which a reset or a
	// close by the far side does in milliseconds.
	go func() {
		_ = c.ssh.Wait()
		_ = c.Close()
	}()
	return c, nil
}

// Dead is closed once this connection can no longer carry anything, however it
// died: the keepalive noticed, the transport failed, or it was closed here.
//
// Published because nothing else can tell. A request handed a dead connection
// fails in a way that reads as the workspace refusing rather than as a tunnel
// that went away, and whatever holds the connection has no other way to learn
// it must open another.
func (c *Client) Dead() <-chan struct{} { return c.done }

// Alive is Dead asked without waiting.
func (c *Client) Alive() bool {
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

// keepAlive probes the connection so a silently dead tunnel is detected in
// seconds rather than whenever something next tries to use it.
//
// The reply is waited for on its own clock, not SendRequest's. SendRequest
// blocks until an answer or until the transport gives up, and a link that has
// stopped carrying anything without breaking -- a NAT idling the flow out, a
// laptop suspended and resumed -- stays writable for as long as TCP keeps
// retransmitting, which is minutes. Config.KeepAlive promises seconds.
//
// The wait is twice the interval, so a workspace briefly too busy to answer is
// not mistaken for a workspace that is gone.
func (c *Client) keepAlive() {
	t := time.NewTicker(c.cfg.KeepAlive)
	defer t.Stop()

	for {
		select {
		case <-c.done:
			return
		case <-t.C:
			// Buffered, so the goroutine writing the answer is never left
			// blocked on a receive that has gone.
			answered := make(chan error, 1)
			go func() {
				_, _, err := c.ssh.SendRequest(tunnel.KeepAliveRequest, true, nil)
				answered <- err
			}()

			select {
			case err := <-answered:
				if err != nil {
					_ = c.Close()
					return
				}
			case <-time.After(2 * c.cfg.KeepAlive):
				// Twice the interval rather than once, so a workspace briefly
				// too busy to answer is not mistaken for one that is gone.
				_ = c.Close()
				return
			case <-c.done:
				return
			}
		}
	}
}

// Close tears the connection down. Safe to call more than once.
func (c *Client) Close() error {
	var err error
	c.once.Do(func() {
		close(c.done)
		err = c.ssh.Close()
	})
	return err
}

// Listen asks the workspace to listen on addr and forward connections back
// here. This is ssh -R, and it is how the client's NFS export reaches the
// remote dockerd.
//
// The workspace will refuse an address that is not this account's. See
// workspace.Mapping.OwnsPort. A refusal here is a policy decision on the far
// side, not a transport failure, and reads as "port already bound" from the
// SSH protocol's point of view.
func (c *Client) Listen(addr string) (net.Listener, error) {
	l, err := c.ssh.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("tunnel: requesting remote listener on %s: %w", addr, err)
	}
	return l, nil
}

// DialRemote opens a connection from the workspace to addr. This is the
// outbound half of ssh -L, used to reach published container ports and the
// Docker socket.
func (c *Client) DialRemote(addr string) (net.Conn, error) {
	conn, err := c.ssh.Dial("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("tunnel: dialling %s from the workspace: %w", addr, err)
	}
	return conn, nil
}

// Run executes a command on the workspace and returns its stdout.
//
// stderr is captured and folded into the error, because a remote command that
// fails with a useful message on stderr and nothing on stdout is the common
// case and losing that message is how the shell clients produced unhelpful
// failures.
func (c *Client) Run(ctx context.Context, cmd string) ([]byte, error) {
	sess, err := c.ssh.NewSession()
	if err != nil {
		return nil, fmt.Errorf("tunnel: opening session: %w", err)
	}
	defer func() { _ = sess.Close() }()

	var stderr strings.Builder
	sess.Stderr = &stderr

	type result struct {
		out []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		out, err := sess.Output(cmd)
		ch <- result{out, err}
	}()

	select {
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGKILL)
		return nil, ctx.Err()
	case r := <-ch:
		if r.err != nil {
			if msg := strings.TrimSpace(stderr.String()); msg != "" {
				return r.out, fmt.Errorf("tunnel: %q: %w: %s", cmd, r.err, msg)
			}
			return r.out, fmt.Errorf("tunnel: %q: %w", cmd, r.err)
		}
		return r.out, nil
	}
}

// OpenStream runs a command and returns pipes to it, for the long-lived
// bidirectional case, notably `docker system dial-stdio`, which carries the
// whole Docker API.
func (c *Client) OpenStream(cmd string) (io.ReadWriteCloser, error) {
	sess, err := c.ssh.NewSession()
	if err != nil {
		return nil, fmt.Errorf("tunnel: opening session: %w", err)
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		_ = sess.Close()
		return nil, fmt.Errorf("tunnel: stdin pipe: %w", err)
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		_ = sess.Close()
		return nil, fmt.Errorf("tunnel: stdout pipe: %w", err)
	}
	if err := sess.Start(cmd); err != nil {
		_ = sess.Close()
		return nil, fmt.Errorf("tunnel: starting %q: %w", cmd, err)
	}
	return &sessionStream{sess: sess, in: stdin, out: stdout}, nil
}

type sessionStream struct {
	sess *ssh.Session
	in   io.WriteCloser
	out  io.Reader
	once sync.Once
}

func (s *sessionStream) Read(p []byte) (int, error)  { return s.out.Read(p) }
func (s *sessionStream) Write(p []byte) (int, error) { return s.in.Write(p) }

// CloseWrite signals end-of-input to the remote command without ending the
// session, by closing only the stdin pipe.
//
// This distinction is load-bearing for `docker run`. Without -i, the CLI
// closes its stdin as soon as the attach is established. A proxy that
// responded by closing the whole stream would tear the session down before
// the container had written anything, and the command would exit cleanly
// having printed nothing at all.
func (s *sessionStream) CloseWrite() error { return s.in.Close() }

func (s *sessionStream) Close() error {
	var err error
	s.once.Do(func() {
		_ = s.in.Close()
		err = s.sess.Close()
	})
	return err
}
