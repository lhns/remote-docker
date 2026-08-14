package config

// How a workspace is reached, worked out from what `host` says.
//
// The scheme is part of the host setting rather than a setting of its own, so
// there is one place that says where a workspace is. A bare host means what it
// meant before schemes were accepted, so existing configuration is unaffected:
//
//	dev.example              ssh, port from `port` (2222)
//	ssh://dev.example:2222   the same, said explicitly
//	wss://ws.example/tunnel  through a reverse proxy
//	ws://inside:8080/tunnel  a WebSocket with no TLS, inside a trusted network
//
// The SSH handshake runs inside a WebSocket as it does over TCP, so ws and wss
// are authenticated and encrypted by SSH in the same way. TLS on a wss
// connection authenticates the proxy in front. See ADR 0034.

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

	// Host and Port are the SSH endpoint, set for every kind: a WebSocket
	// still names a host, and the SSH layer above still wants something to
	// call the far side when it reports where it connected.
	Host string
	Port int

	// URL is the endpoint to dial, empty for plain SSH.
	URL string
}

// WebSocket reports whether this transport goes through an HTTP proxy.
func (t Transport) WebSocket() bool { return t.Kind == TransportWS || t.Kind == TransportWSS }

// String is what `inspect` prints: enough to see which way in is being used.
func (t Transport) String() string {
	if t.WebSocket() {
		return t.URL
	}
	return fmt.Sprintf("ssh://%s", net.JoinHostPort(t.Host, strconv.Itoa(t.Port)))
}

// Transport works out how this workspace is reached.
//
// A disagreement is refused rather than resolved. A config naming two different
// ports is a mistake, and picking one silently is how it survives long enough
// to confuse somebody.
func (c Config) Transport() (Transport, error) {
	host := strings.TrimSpace(c.Host)
	if host == "" {
		return Transport{}, fmt.Errorf("config: no host")
	}

	scheme, rest, ok := splitScheme(host)
	if !ok {
		// A bare host, which is what every config held before any of this and
		// what most hold now.
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
			// A machine is started on demand and told its address at boot
			// (ADR 0026), so it is reached over ssh at whatever that turned out
			// to be. A WebSocket endpoint for one is not unsupported so much as
			// incoherent, and silently ignoring one half would hide the mistake.
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
		// Explicit rather than implied: the request goes to / either way. The
		// agent ignores the path; it is there for a proxy that routes on one.
		if u.Path == "" {
			u.Path = "/"
		}
		return Transport{Kind: scheme, Host: u.Hostname(), Port: port, URL: u.String()}, nil
	}

	return Transport{}, fmt.Errorf(
		"config: host %q names %q, which is not a way to reach a workspace (ssh, ws or wss)", c.Host, scheme)
}

// isWebSocketHost reports whether a host names a WebSocket endpoint, which is
// what decides whether the SSH port default applies to it.
func isWebSocketHost(host string) bool {
	scheme, _, ok := splitScheme(strings.TrimSpace(host))
	return ok && (scheme == TransportWS || scheme == TransportWSS)
}

// splitScheme separates "scheme://rest". Reported rather than guessed: a host
// with a colon in it is a port, not a scheme.
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
