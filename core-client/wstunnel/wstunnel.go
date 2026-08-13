// Package wstunnel dials the workspace through an HTTP reverse proxy.
//
// The SSH session is unchanged: this only decides what carries it. A workspace
// behind a reverse proxy is reachable on 443 like anything else that proxy
// fronts, which is the whole point -- an open SSH port is the thing that makes a
// workspace hard to get to from a network that allows little else.
//
// TLS here proves which FRONT DOOR was reached, and nothing more. The SSH host
// key still proves this is the workspace and the client key still proves which
// machine is calling, both inside the tunnel, so a proxy in the path sees
// ciphertext it can neither read nor forge. That is why Insecure exists and why
// ws:// is offered at all: neither makes the session unauthenticated.
package wstunnel

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

// Options describe one workspace's WebSocket endpoint.
type Options struct {
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

	// Timeout bounds the HTTP handshake, not the session that follows.
	Timeout time.Duration
}

const defaultTimeout = 30 * time.Second

// Dialer returns a function tunnelclient.Config.Dial can use.
func Dialer(opts Options) (func(ctx context.Context) (net.Conn, error), error) {
	if opts.Timeout == 0 {
		opts.Timeout = defaultTimeout
	}

	tlsCfg, err := tlsConfig(opts)
	if err != nil {
		return nil, err
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

		return websocket.NetConn(context.Background(), c, websocket.MessageBinary), nil
	}, nil
}

// hint turns the two failures worth naming into something actionable. A proxy
// that routed the request somewhere else answers with an ordinary status, and
// "bad handshake" alone sends people looking at the wrong end.
func hint(resp *http.Response) string {
	if resp == nil {
		return ""
	}
	switch resp.StatusCode {
	case http.StatusNotFound:
		return "\n\tfix: the proxy answered 404; check the path matches the agent's --ws-path"
	case http.StatusBadGateway, http.StatusServiceUnavailable:
		return "\n\tfix: the proxy could not reach the agent; check its --ws-addr and the route"
	}
	return ""
}

func tlsConfig(opts Options) (*tls.Config, error) {
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
