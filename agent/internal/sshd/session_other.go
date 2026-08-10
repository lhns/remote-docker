//go:build !linux

package sshd

import (
	"fmt"

	gssh "github.com/gliderlabs/ssh"
)

// handleSession refuses on platforms that cannot drop privilege the way the
// agent requires.
//
// The agent runs as PID 1 in a Linux container and nowhere else; running
// commands as the authenticated account needs syscall.Credential, which only
// Linux has. This exists so the package still compiles on a development
// machine. The tests that matter, the forward policy and the account store,
// are portable and run everywhere.
func (s *Server) handleSession(session gssh.Session) {
	_, _ = fmt.Fprintln(session.Stderr(), "the remote-docker agent only runs on Linux")
	_ = session.Exit(1)
}
