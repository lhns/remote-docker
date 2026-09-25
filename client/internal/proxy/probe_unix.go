//go:build !windows

package proxy

import (
	"cmp"
	"context"
	"net"
	"time"
)

// Reachable reports whether something is serving the endpoint right now, to
// tell "no session" from "broken session" where both say ENOENT.
func Reachable(endpoint string) bool {
	endpoint = cmp.Or(endpoint, DefaultEndpoint())
	conn, err := net.DialTimeout("unix", endpoint, time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// DialEndpoint dials the endpoint, for an http.Transport talking to a session's
// own control endpoints. The URL's host is ignored; only this matters.
func DialEndpoint(endpoint string) func(context.Context, string, string) (net.Conn, error) {
	endpoint = cmp.Or(endpoint, DefaultEndpoint())
	return func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", endpoint)
	}
}
