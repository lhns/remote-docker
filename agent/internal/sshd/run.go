package sshd

import (
	gssh "github.com/gliderlabs/ssh"
	"golang.org/x/crypto/ssh"

	"github.com/lhns/remote-docker/core/workspace"
)

// handleRun answers workspace.RunRequest (ADR 0050): an ephemeral account's
// connection takes the run's client, once; any other account's is acknowledged
// and unchanged. A refusal's payload is the reason, which the client prints.
//
// Any number of connections may name one run, which is how a one-off command
// joins the background session's; the reverse forward is what only one of
// them can hold.
func (s *Server) handleRun(ctx gssh.Context, _ *gssh.Server, req *ssh.Request) (bool, []byte) {
	account, ok := accountFor(ctx)
	if !ok {
		return false, []byte("this connection has no account")
	}
	if !account.ephemeral {
		return true, nil
	}

	refuse := func(why string) (bool, []byte) {
		s.log().Warn("refused a run", "why", why, "account", account.Name(), "from", ctx.RemoteAddr())
		return false, []byte(why)
	}
	if account.client != "" {
		return refuse("this connection already named its run")
	}
	run := string(req.Payload)
	if !workspace.ValidRunID(run) {
		return refuse("the run id is malformed")
	}
	account.client = workspace.EphemeralClientID(account.key, run)
	ctx.SetValue(contextKey{}, account)
	return true, nil
}
