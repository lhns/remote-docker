// Package wslisten serves SSH over a WebSocket, so a workspace can be reached
// through an ordinary HTTP reverse proxy.
//
// It is a net.Listener and nothing more: connections that arrive as WebSocket
// upgrades are handed to whatever accepts them, which is the same SSH server
// that accepts TCP. Everything above the transport -- authentication, the
// forwards, sessions -- neither knows nor cares which one it got.
//
// NO TLS HERE, deliberately. The proxy terminates it. An agent that owned a
// certificate would eventually present an expired one, and that presents as a
// workspace being unreachable for a reason nothing on screen names. Serving
// plaintext is not the weakness it looks like: the same SSH handshake runs
// inside, so this door has the same lock as the TCP one.
package wslisten

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/lhns/remote-docker/core/logx"
)

// PeerTimeout is how long a connection may fail to answer before it is dropped.
//
// This is the WebSocket's own liveness, and it is not optional. The TCP-level
// detection the agent applies elsewhere (sshd.armDeadPeerDetection) works on a
// *net.TCPConn, and what arrives here is a WebSocket wrapping one, so none of it
// applies. Without this, a client that vanishes keeps its reverse-tunnel port
// reserved -- and the symptom is not a lost connection but a REFUSED FORWARD on
// some later reconnect, with containers mounting against a port bound to
// nothing.
//
// It doubles as what keeps the tunnel alive through a proxy's idle timeout,
// which is why the interval is well under any default worth caring about.
const PeerTimeout = 60 * time.Second

// pingInterval leaves room for two missed answers inside PeerTimeout.
const pingInterval = PeerTimeout / 3

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

// keepAlive drops a connection that stops answering. See PeerTimeout.
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

// New starts listening on addr and serves the upgrade endpoint at path.
//
// The listener is returned rather than served here, so the caller decides what
// accepts from it -- which is the same SSH server that accepts TCP.
func New(addr, path string, log *slog.Logger) (*Server, error) {
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

	mux := http.NewServeMux()
	mux.Handle(path, l.Handler())
	// Anything else is a proxy misconfiguration, and answering 404 says so more
	// clearly than a hanging request.
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not the tunnel endpoint", http.StatusNotFound)
	})

	s := &Server{
		Listener: l,
		tcp:      tcp,
		http: &http.Server{
			Handler: mux,
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
