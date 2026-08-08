package sshx

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
)

// Config describes how to reach a workspace.
type Config struct {
	Host string
	Port int
	User string

	Key        KeyPair
	KnownHosts *KnownHosts

	// Ciphers, if set, replaces the negotiated cipher list.
	//
	// aes128-gcm is the default for a reason worth keeping: AES-NI makes it
	// several GB/s where ChaCha20 is markedly slower, and every byte of the
	// NFS traffic crosses this connection. There is no double encryption to
	// worry about -- NFS inside the tunnel is plaintext.
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
//
// One Client carries every channel this tool needs: the reverse forward for
// the NFS export, a local forward per published container port, the Docker API
// stream, and any interactive session. That multiplexing is inherent to SSH
// and is why the ControlMaster split between the old clients disappears.
type Client struct {
	ssh  *ssh.Client
	cfg  Config
	once sync.Once
	done chan struct{}
}

// Dial connects and authenticates.
func Dial(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.KnownHosts == nil {
		return nil, errors.New("sshx: Config.KnownHosts is required")
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
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(cfg.Key.Signer)},
		HostKeyCallback: cfg.KnownHosts.Callback(),
		Timeout:         cfg.Timeout,
	}
	clientCfg.Ciphers = cfg.Ciphers

	dialer := &net.Dialer{Timeout: cfg.Timeout}
	conn, err := dialer.DialContext(ctx, "tcp", cfg.Addr())
	if err != nil {
		return nil, fmt.Errorf("sshx: dialling %s: %w", cfg.Addr(), err)
	}

	sshConn, chans, reqs, err := ssh.NewClientConn(conn, cfg.Addr(), clientCfg)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("sshx: connecting to %s@%s: %w", cfg.User, cfg.Addr(), err)
	}

	c := &Client{
		ssh:  ssh.NewClient(sshConn, chans, reqs),
		cfg:  cfg,
		done: make(chan struct{}),
	}
	go c.keepAlive()
	return c, nil
}

// keepAlive probes the connection so a silently dead tunnel is detected in
// seconds rather than whenever something next tries to use it.
func (c *Client) keepAlive() {
	t := time.NewTicker(c.cfg.KeepAlive)
	defer t.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-t.C:
			if _, _, err := c.ssh.SendRequest("keepalive@openssh.com", true, nil); err != nil {
				_ = c.Close()
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
// The workspace will refuse an address that is not this account's -- see
// workspace.Mapping.OwnsPort. A refusal here is a policy decision on the far
// side, not a transport failure, and reads as "port already bound" from the
// SSH protocol's point of view.
func (c *Client) Listen(addr string) (net.Listener, error) {
	l, err := c.ssh.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("sshx: requesting remote listener on %s: %w", addr, err)
	}
	return l, nil
}

// DialRemote opens a connection from the workspace to addr. This is the
// outbound half of ssh -L, used to reach published container ports and the
// Docker socket.
func (c *Client) DialRemote(addr string) (net.Conn, error) {
	conn, err := c.ssh.Dial("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("sshx: dialling %s from the workspace: %w", addr, err)
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
		return nil, fmt.Errorf("sshx: opening session: %w", err)
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
				return r.out, fmt.Errorf("sshx: %q: %w: %s", cmd, r.err, msg)
			}
			return r.out, fmt.Errorf("sshx: %q: %w", cmd, r.err)
		}
		return r.out, nil
	}
}

// OpenStream runs a command and returns pipes to it, for the long-lived
// bidirectional case -- notably `docker system dial-stdio`, which carries the
// whole Docker API.
func (c *Client) OpenStream(cmd string) (io.ReadWriteCloser, error) {
	sess, err := c.ssh.NewSession()
	if err != nil {
		return nil, fmt.Errorf("sshx: opening session: %w", err)
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		_ = sess.Close()
		return nil, fmt.Errorf("sshx: stdin pipe: %w", err)
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		_ = sess.Close()
		return nil, fmt.Errorf("sshx: stdout pipe: %w", err)
	}
	if err := sess.Start(cmd); err != nil {
		_ = sess.Close()
		return nil, fmt.Errorf("sshx: starting %q: %w", cmd, err)
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
