//go:build windows

package proxy

import (
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
		endpoint = DefaultEndpoint
	}
	timeout := time.Second
	conn, err := winio.DialPipe(endpoint, &timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
