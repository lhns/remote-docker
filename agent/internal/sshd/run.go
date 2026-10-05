package sshd

import (
	"fmt"

	gssh "github.com/gliderlabs/ssh"
	"golang.org/x/crypto/ssh"

	"github.com/lhns/remote-docker/core/cache"
	"github.com/lhns/remote-docker/core/notify"
	"github.com/lhns/remote-docker/core/workspace"
)

// handleRun answers workspace.RunRequest (ADR 0050). For an ephemeral account
// it makes the connection's client the run's, once; any other account's is
// acknowledged and changes nothing. A refusal's payload is the reason, which
// the client prints.
func (s *Server) handleRun(ctx gssh.Context, _ *gssh.Server, req *ssh.Request) (bool, []byte) {
	account, ok := accountFor(ctx)
	if !ok {
		return false, nil
	}
	if !account.ephemeral {
		return true, nil
	}

	refuse := func(why string) (bool, []byte) {
		s.log().Warn("refused a run: "+why, "account", account.Name(), "from", ctx.RemoteAddr())
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

	if !s.claimRun(ctx, account) {
		return refuse(fmt.Sprintf("run %s of account %s already has a live connection\n"+
			"  fix: close it, or wait about a minute for the workspace to notice it is gone",
			account.client, account.Name()))
	}
	ctx.SetValue(contextKey{}, account)
	s.log().Info("a run connected", "account", account.Name(), "client", account.client)
	return true, nil
}

// claimRun records this connection as the run's one live connection, until the
// connection ends.
func (s *Server) claimRun(ctx gssh.Context, account sessionAccount) bool {
	key := runKey{account.Name(), account.client}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runs[key] {
		return false
	}
	if s.runs == nil {
		s.runs = map[runKey]bool{}
	}
	s.runs[key] = true

	go func() {
		<-ctx.Done()
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.runs, key)
	}()
	return true
}

// clientRefusal is why a request that needs the connection's client cannot
// have it, or "" when it can: an ephemeral account's connection that has not
// named its run.
func clientRefusal(account sessionAccount) string {
	if account.client != "" {
		return ""
	}
	return fmt.Sprintf("account %s gives each client run its own identity, and this connection named no run\n"+
		"  fix: upgrade remote-docker on this machine", account.Name())
}

// needsClient reports whether a session command depends on which client asks:
// its port, its volumes or its unions.
func needsClient(command string) bool {
	switch command {
	case workspace.InfoCommand, notify.Command, cache.Command:
		return true
	}
	return false
}
