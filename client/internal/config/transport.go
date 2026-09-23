package config

// How a workspace is reached, from the scheme in `host` (ADR 0034):
//
//	dev.example              ssh, port from `port` (2222)
//	ssh://dev.example:2222   the same, said explicitly
//	wss://ws.example/tunnel  through a reverse proxy
//	ws://inside:8080/tunnel  a WebSocket with no TLS, inside a trusted network
//
// SSH runs inside the WebSocket, so ws is still authenticated and encrypted.

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// Transport kinds.
const (
	TransportSSH = "ssh"
	TransportWS  = "ws"
	TransportWSS = "wss"
)

// Transport says how to reach a workspace: an SSH endpoint, or a URL to open a
// WebSocket to.
type Transport struct {
	// Kind is TransportSSH, TransportWS or TransportWSS.
	Kind string

	// Host and Port name the far side, set for every kind.
	Host string
	Port int

	// URL is the endpoint to dial, empty for plain SSH.
	URL string
}

// WebSocket reports whether this transport goes through an HTTP proxy.
func (t Transport) WebSocket() bool { return t.Kind == TransportWS || t.Kind == TransportWSS }

// String is what `inspect` prints.
func (t Transport) String() string {
	if t.WebSocket() {
		return t.URL
	}
	return fmt.Sprintf("ssh://%s", net.JoinHostPort(t.Host, strconv.Itoa(t.Port)))
}

// Transport works out how this workspace is reached, refusing a config that
// contradicts itself rather than picking one side.
func (c Config) Transport() (Transport, error) {
	host := strings.TrimSpace(c.Host)
	if host == "" {
		return Transport{}, fmt.Errorf("config: no host")
	}

	scheme, rest, ok := splitScheme(host)
	if !ok {
		return Transport{Kind: TransportSSH, Host: host, Port: c.portOr(DefaultSSHPort)}, nil
	}

	switch scheme {
	case TransportSSH:
		u, err := parse(scheme, rest)
		if err != nil {
			return Transport{}, err
		}
		port, err := c.portFor(u, DefaultSSHPort)
		if err != nil {
			return Transport{}, err
		}
		return Transport{Kind: TransportSSH, Host: u.Hostname(), Port: port}, nil

	case TransportWS, TransportWSS:
		if c.Machine != nil {
			// A machine is reached over ssh at its boot address (ADR 0026).
			return Transport{}, fmt.Errorf(
				"config: a machine workspace is reached over ssh, so host %q cannot be a WebSocket", c.Host)
		}
		u, err := parse(scheme, rest)
		if err != nil {
			return Transport{}, err
		}
		port, err := c.portFor(u, defaultPortFor(scheme))
		if err != nil {
			return Transport{}, err
		}
		// The agent ignores the path; a proxy may route on it.
		if u.Path == "" {
			u.Path = "/"
		}
		return Transport{Kind: scheme, Host: u.Hostname(), Port: port, URL: u.String()}, nil
	}

	return Transport{}, fmt.Errorf(
		"config: host %q names %q, which is not a way to reach a workspace (ssh, ws or wss)", c.Host, scheme)
}

// isWebSocketHost reports whether a host names a WebSocket endpoint, to which
// the SSH port default does not apply.
func isWebSocketHost(host string) bool {
	scheme, _, ok := splitScheme(strings.TrimSpace(host))
	return ok && (scheme == TransportWS || scheme == TransportWSS)
}

// splitScheme separates "scheme://rest"; a bare colon is a port, not a scheme.
func splitScheme(host string) (scheme, rest string, ok bool) {
	scheme, rest, ok = strings.Cut(host, "://")
	if !ok {
		return "", host, false
	}
	return strings.ToLower(scheme), rest, true
}

func parse(scheme, rest string) (*url.URL, error) {
	u, err := url.Parse(scheme + "://" + rest)
	if err != nil {
		return nil, fmt.Errorf("config: host %s://%s: %w", scheme, rest, err)
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("config: host %s://%s names no host", scheme, rest)
	}
	return u, nil
}

func defaultPortFor(scheme string) int {
	if scheme == TransportWSS {
		return 443
	}
	return 80
}

// portFor decides the port, refusing a `port` setting that contradicts the URL.
func (c Config) portFor(u *url.URL, fallback int) (int, error) {
	inURL := 0
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("config: host %s names port %q, which is not a port", u.Redacted(), p)
		}
		inURL = n
	}

	switch {
	case inURL != 0 && c.Port != 0 && c.Port != inURL:
		return 0, fmt.Errorf(
			"config: host %s names port %d and `port` says %d; remove one", u.Redacted(), inURL, c.Port)
	case inURL != 0:
		return inURL, nil
	default:
		return c.portOr(fallback), nil
	}
}

func (c Config) portOr(fallback int) int {
	if c.Port != 0 {
		return c.Port
	}
	return fallback
}
