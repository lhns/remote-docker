package sshd

import (
	"context"
	"fmt"

	gssh "github.com/gliderlabs/ssh"
	"golang.org/x/crypto/ssh"

	"github.com/lhns/remote-docker/core/cache"
	"github.com/lhns/remote-docker/core/notify"
	"github.com/lhns/remote-docker/core/workspace"
)

// handleRun answers workspace.RunRequest (ADR 0050): an ephemeral account's
// connection takes the run's client, once; any other account's is acknowledged
// and unchanged. A refusal's payload is the reason, which the client prints.
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

	if !s.claimRun(ctx, runKey{account.Name(), account.client}) {
		return refuse(fmt.Sprintf("run %s of account %s already has a live connection\n"+
			"  fix: close it, or wait about a minute for the workspace to notice it is gone",
			account.client, account.Name()))
	}
	ctx.SetValue(contextKey{}, account)
	s.log().Info("a run connected", "account", account.Name(), "client", account.client)
	return true, nil
}

// claimRun makes this connection the run's one live connection, until it ends.
func (s *Server) claimRun(ctx context.Context, key runKey) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, taken := s.runs[key]; taken {
		return false
	}
	if s.runs == nil {
		s.runs = map[runKey]struct{}{}
	}
	s.runs[key] = struct{}{}

	context.AfterFunc(ctx, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.runs, key)
	})
	return true
}

// needsClient reports whether a session command depends on which client asks:
// its port, its volumes or its unions. An ephemeral connection that has not
// named its run has no client to give it.
func needsClient(command string) bool {
	switch command {
	case workspace.InfoCommand, notify.Command, cache.Command:
		return true
	}
	return false
}
