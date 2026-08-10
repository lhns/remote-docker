//go:build windows

package proxy

import (
	"context"
	"net"
	"time"

	"github.com/Microsoft/go-winio"
)

// Reachable reports whether something is serving the endpoint right now.
//
// Used to tell "no session is running" apart from "the session is broken",
// which the underlying error cannot: a named pipe that nobody has created and
// a path that is genuinely wrong both surface as "The system cannot find the
// file specified", and the user can act on one of those and not the other.
func Reachable(endpoint string) bool {
	if endpoint == "" {
		endpoint = defaultPipe
	}
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
	if endpoint == "" {
		endpoint = defaultPipe
	}
	return func(ctx context.Context, _, _ string) (net.Conn, error) {
		timeout := 10 * time.Second
		if deadline, ok := ctx.Deadline(); ok {
			timeout = time.Until(deadline)
		}
		return winio.DialPipe(endpoint, &timeout)
	}
}
