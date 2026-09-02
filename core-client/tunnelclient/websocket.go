package tunnelclient

// Reaching a workspace through an HTTP reverse proxy: a WebSocket returned as
// a net.Conn, which is what Config.Dial takes.
//
// In this package rather than beside it, because dialling the tunnel over TCP
// and dialling it through a proxy are one subject, and the seam between them
// was a single function. Used when the workspace is behind an HTTP reverse
// proxy, which is how one is reached without an SSH port open to it.
//
// TLS on this connection authenticates the proxy. It does not authenticate the
// workspace: the SSH host key does that, inside the tunnel, and the client key
// identifies the machine. This is why Insecure and ws:// are offered -- both
// give up checking which proxy answered, and neither affects whether the SSH
// session itself is authenticated and encrypted. See ADR 0034.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/coder/websocket"
)

// WebSocketOptions describe one workspace's WebSocket endpoint.
type WebSocketOptions struct {
	// URL is the endpoint, ws:// or wss://.
	URL string

	// CAFile verifies the server against a private CA instead of the system
	// roots. Empty uses the system roots, which is what a proxy holding an
	// ordinary public certificate wants.
	CAFile string

	// Insecure accepts any server certificate, for a proxy holding a
	// self-signed one. It gives up knowing which proxy answered; it does not
	// give up authentication, which SSH does inside.
	Insecure bool

	// Addr is the host:port the connection reports as its remote address.
	//
	// Required, and not derived from URL, because the caller already worked it
	// out: deriving it here would put "wss means 443" in a second place, free
	// to disagree with the first. It matters because known_hosts looks a host
	// key up by this address, and the WebSocket library reports a placeholder
	// with no port in it.
	Addr string

	// Timeout bounds the HTTP handshake, not the session that follows.
	Timeout time.Duration
}

// WebSocketDialer returns a function tunnelclient.Config.Dial can use.
func WebSocketDialer(opts WebSocketOptions) (func(ctx context.Context) (net.Conn, error), error) {
	if opts.Timeout == 0 {
		opts.Timeout = defaultTimeout
	}

	tlsCfg, err := tlsConfig(opts)
	if err != nil {
		return nil, err
	}

	if opts.Addr == "" {
		return nil, fmt.Errorf("wstunnel: WebSocketOptions.Addr is required, or no host key can be checked")
	}

	// One client, and therefore one connection pool, for every dial to this
	// workspace.
	client := &http.Client{
		Timeout: opts.Timeout,
		Transport: &http.Transport{
			TLSClientConfig: tlsCfg,
			// The tunnel is one long-lived connection; a proxy that answers the
			// upgrade slowly is still answering.
			ResponseHeaderTimeout: opts.Timeout,
		},
	}

	return func(ctx context.Context) (net.Conn, error) {
		// The handshake is bounded, the session is not: the returned net.Conn
		// outlives this context, so it must not be the one that cancels it.
		hctx, cancel := context.WithTimeout(ctx, opts.Timeout)
		defer cancel()

		c, resp, err := websocket.Dial(hctx, opts.URL, &websocket.DialOptions{
			HTTPClient: client,
		})
		if err != nil {
			return nil, fmt.Errorf("wstunnel: dialling %s: %w%s", opts.URL, err, hint(resp))
		}

		// No read limit. The default is 32KiB per message, and this carries a
		// stream rather than messages: a large SSH packet would be refused as
		// oversized, which reads as the tunnel breaking under load.
		c.SetReadLimit(-1)

		// The address matters as well as the bytes. known_hosts looks a host
		// key up by the connection's RemoteAddr, and the library reports
		// "websocket/unknown-addr", which has no port in it, so every host-key
		// check fails before it can compare anything.
		return &addrConn{
			Conn:   websocket.NetConn(context.Background(), c, websocket.MessageBinary),
			remote: wsAddr(opts.Addr),
		}, nil
	}, nil
}

// addrConn reports a real host:port for a connection that has none of its own.
type addrConn struct {
	net.Conn
	remote net.Addr
}

func (c *addrConn) RemoteAddr() net.Addr { return c.remote }

// wsAddr is the endpoint a WebSocket was dialled at, in the form anything
// parsing an address expects.
type wsAddr string

func (a wsAddr) Network() string { return "tcp" }
func (a wsAddr) String() string  { return string(a) }

// hint turns the two failures worth naming into something actionable. A proxy
// that routed the request somewhere else answers with an ordinary status, and
// "bad handshake" alone sends people looking at the wrong end.
func hint(resp *http.Response) string {
	if resp == nil {
		return ""
	}
	switch resp.StatusCode {
	case http.StatusNotFound:
		// Not about the path: the agent takes the upgrade on any path
		// (ADR 0034). A 404 is the proxy having no route to it, which is what
		// a restarting or absent agent looks like from in front.
		return "\n\tfix: the proxy answered 404, so it has no route to the agent; check the route and that the agent is up"
	case http.StatusBadGateway, http.StatusServiceUnavailable:
		return "\n\tfix: the proxy could not reach the agent; check its --ws-addr and the route"
	}
	return ""
}

func tlsConfig(opts WebSocketOptions) (*tls.Config, error) {
	if opts.Insecure {
		// #nosec G402 -- deliberate and per workspace. SSH inside still
		// authenticates both ends; see the package comment.
		return &tls.Config{InsecureSkipVerify: true}, nil
	}
	if opts.CAFile == "" {
		return nil, nil
	}

	pem, err := os.ReadFile(opts.CAFile)
	if err != nil {
		return nil, fmt.Errorf("wstunnel: reading %s: %w", opts.CAFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		// A file that exists and holds no certificate is a configuration
		// mistake, and falling back to the system roots would hide it behind a
		// connection that works until the day it should not have.
		return nil, fmt.Errorf("wstunnel: %s holds no certificate", opts.CAFile)
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, nil
}
