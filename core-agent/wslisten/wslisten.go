// Package wslisten accepts WebSocket connections and presents them as a
// net.Listener, so the SSH server can accept from a reverse proxy as well as
// from a TCP port.
//
// Connections are handed to whatever calls Accept, which is the same SSH server
// that accepts TCP. Nothing above the transport is told which kind it got.
//
// This package does not do TLS, and the agent has no certificate options at
// all. The proxy in front terminates TLS. The traffic between the proxy and the
// agent is an ordinary SSH handshake, so it is authenticated and encrypted by
// SSH even though the WebSocket carrying it is not. See ADR 0034.
package wslisten

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/lhns/remote-docker/core/logx"
)

// peerTimeout is how long a connection may fail to answer a ping before it is
// dropped.
//
// The agent's other dead-peer detection (sshd.armDeadPeerDetection) sets TCP
// options on a *net.TCPConn. A connection arriving here is a WebSocket wrapping
// one, so those options are never set and this ping is the only thing that
// notices a client that stopped responding.
//
// It matters because a connection that is never dropped keeps its
// reverse-tunnel port reserved. The next session from that machine is then
// refused its forward, and containers mount against a port with nothing behind
// it, which does not look like a transport problem at all.
//
// The ping also keeps the connection from being closed by a proxy's idle
// timeout.
const peerTimeout = 60 * time.Second

// pingInterval leaves room for two missed answers inside peerTimeout.
const pingInterval = peerTimeout / 3

// Listener accepts WebSocket connections as if they were ordinary ones.
type Listener struct {
	addr net.Addr
	log  *slog.Logger

	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once

	// ping overrides how often a connection is probed. Zero means
	// pingInterval; a test sets it small, because the real one is measured in
	// tens of seconds and the behaviour under test is what happens when an
	// answer never comes.
	ping time.Duration
}

func (l *Listener) pingEvery() time.Duration {
	if l.ping > 0 {
		return l.ping
	}
	return pingInterval
}

// Handler upgrades a request and hands the connection on. Mount it wherever the
// proxy is configured to send the tunnel.
func (l *Listener) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A plain request -- a browser, a health check -- is answered rather
		// than left to hang.
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			http.Error(w, "remote-docker: this is an ssh tunnel endpoint; connect with ws:// or wss://",
				http.StatusUpgradeRequired)
			return
		}

		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			// The client is not a browser and there is no origin to check;
			// authentication is the SSH handshake that follows, not this.
			InsecureSkipVerify: true,
		})
		if err != nil {
			l.log.Warn("refused a websocket upgrade", "err", err, "from", r.RemoteAddr)
			return
		}

		// No read limit: this carries a stream, and the 32KiB default would
		// refuse a large SSH packet as oversized, which reads as the tunnel
		// breaking under load.
		c.SetReadLimit(-1)

		// The connection outlives this request, so it gets a context of its own
		// rather than the request's, which the server cancels on return.
		ctx, cancel := context.WithCancel(context.Background())
		conn := websocket.NetConn(ctx, c, websocket.MessageBinary)
		go l.keepAlive(ctx, cancel, c)

		select {
		case l.conns <- conn:
			// Held open until the accepting side is done with it. Returning
			// from the handler would let net/http tear the hijacked connection
			// down underneath the SSH session riding it.
			<-ctx.Done()
		case <-l.closed:
			cancel()
			_ = c.Close(websocket.StatusGoingAway, "shutting down")
		}
	})
}

// keepAlive drops a connection that stops answering. See peerTimeout.
func (l *Listener) keepAlive(ctx context.Context, cancel context.CancelFunc, c *websocket.Conn) {
	every := l.pingEvery()
	t := time.NewTicker(every)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, pcancel := context.WithTimeout(ctx, every)
			err := c.Ping(pctx)
			pcancel()
			if err == nil {
				continue
			}
			if ctx.Err() != nil {
				return
			}
			l.log.Info("dropping a websocket that stopped answering", "err", err)
			// CloseNow rather than Close: the peer is not answering, so waiting
			// for it to acknowledge a close is waiting for the thing that
			// already failed.
			_ = c.CloseNow()
			cancel()
			return
		}
	}
}

// Accept implements net.Listener.
func (l *Listener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

// Close stops accepting. Connections already handed out are left alone: they
// belong to whoever accepted them.
func (l *Listener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

// Addr implements net.Listener.
func (l *Listener) Addr() net.Addr { return l.addr }

// Server is the HTTP server and the listener it feeds.
type Server struct {
	Listener *Listener

	http *http.Server
	tcp  net.Listener
}

// New starts listening on addr and accepts WebSocket upgrades on any path.
//
// Any path, so the proxy's route and the agent need not agree on one: a proxy
// that strips its prefix before forwarding and one that does not both work.
//
// The listener is returned rather than served here, so the caller can hand it
// to the SSH server that already accepts TCP connections.
func New(addr string, log *slog.Logger) (*Server, error) {
	log = logx.Or(log)

	tcp, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}

	l := &Listener{
		addr:   tcp.Addr(),
		log:    log,
		conns:  make(chan net.Conn),
		closed: make(chan struct{}),
	}

	s := &Server{
		Listener: l,
		tcp:      tcp,
		http: &http.Server{
			Handler: l.Handler(),
			// No write or read timeout: a tunnel is one request that lasts as
			// long as the session. ReadHeaderTimeout still bounds the part
			// before the upgrade, which is the part an idle attacker holds.
			ReadHeaderTimeout: 30 * time.Second,
		},
	}

	go func() {
		if err := s.http.Serve(tcp); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Warn("the websocket listener stopped", "err", err)
		}
	}()
	return s, nil
}

// Addr reports the address actually bound, which matters when the configured
// one had no port.
func (s *Server) Addr() string { return s.tcp.Addr().String() }

// Close stops the server and the listener.
func (s *Server) Close() error {
	_ = s.Listener.Close()
	return s.http.Close()
}
