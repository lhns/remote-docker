//go:build !windows

package proxy

import (
	"net"
	"time"
)

// Reachable reports whether something is serving the endpoint right now.
//
// Used to tell "no session is running" apart from "the session is broken",
// which the underlying error cannot: a socket nobody has bound and a path that
// is genuinely wrong both surface as "no such file or directory", and the user
// can act on one of those and not the other.
func Reachable(endpoint string) bool {
	if endpoint == "" {
		endpoint = defaultSocketPath()
	}
	conn, err := net.DialTimeout("unix", endpoint, time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
