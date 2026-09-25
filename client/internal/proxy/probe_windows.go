//go:build windows

package proxy

import (
	"cmp"
	"context"
	"net"
	"time"

	"github.com/Microsoft/go-winio"
)

// Reachable reports whether something is serving the endpoint right now, to
// tell "no session" from "broken session" where both say file not found.
func Reachable(endpoint string) bool {
	endpoint = cmp.Or(endpoint, DefaultEndpoint())
	timeout := time.Second
	conn, err := winio.DialPipe(endpoint, &timeout)
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
		timeout := 10 * time.Second
		if deadline, ok := ctx.Deadline(); ok {
			timeout = time.Until(deadline)
		}
		return winio.DialPipe(endpoint, &timeout)
	}
}
