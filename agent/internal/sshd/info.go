package sshd

import (
	"context"
	"time"

	"github.com/lhns/remote-docker/agent/internal/dockercli"
	"github.com/lhns/remote-docker/agent/internal/unions"
	"github.com/lhns/remote-docker/core/workspace"
)

// The daemon queries folded into an info reply.
//
// workspace-info is the client's FIRST round trip, and these come from asking a
// daemon that may still be booting: Warm fires at authentication and this is
// the very next thing the client asks. So every query here uses Lookup and
// never Ensure, and a daemon that is not up is an ordinary answer rather than a
// wait. The values are displayed, not acted upon, and the next command starts
// the daemon. routing_test.go pins it; a careless unification destroys it
// silently, since everything still works, just slowly, on every first
// connection after a restart.

// infoQueryTimeout bounds each query: a daemon slow enough to sit on `docker
// info` is exactly the daemon whose client should not be blocked introducing
// itself.
const infoQueryTimeout = 5 * time.Second

// infoField asks the account's daemon one --format question, and answers
// fallback when the daemon is not up, or does not answer in time.
func (s *Server) infoField(ctx context.Context, account, fallback string, args ...string) string {
	ctx, cancel := context.WithTimeout(ctx, infoQueryTimeout)
	defer cancel()

	target, ok := s.cfg.Daemons.Lookup(ctx, account)
	if !ok {
		return fallback
	}
	out, err := dockercli.CLI{Host: target.Host}.Line(ctx, args...)
	if err != nil {
		return fallback
	}
	return out
}

// dockerVersion asks the account's daemon what it is. Unavailable is a normal
// answer: the client shows it rather than refusing to start.
func (s *Server) dockerVersion(ctx context.Context, account string) string {
	return s.infoField(ctx, account, workspace.DockerUnavailable, dockercli.ServerVersionArgs()...)
}

// storageDriver reports the graph driver of the daemon serving this account.
//
// Worth carrying because the difference between overlay2 and vfs is the
// difference between `docker run` taking a second and taking minutes, nothing
// about it fails, and the account cannot look for itself: reaching the daemon's
// own host is precisely what it may not do.
func (s *Server) storageDriver(ctx context.Context, account string) string {
	return s.infoField(ctx, account, "", "info", "--format", "{{.Driver}}")
}

// unionCapability reports whether this workspace can serve a delegated share as
// a cache, for the account asking.
//
// Asked of the daemon that would serve it rather than of the agent, because
// that is where the answer differs: in per-account mode fuse-overlayfs has to
// be in the image THAT daemon runs, and the agent's own filesystem says nothing
// about it. A daemon that has not started cannot be asked, and an empty answer
// reads as "not available", which is what an older agent's answer reads as too.
func (s *Server) unionCapability(ctx context.Context, account string) string {
	if s.cfg.Unions == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, infoQueryTimeout)
	defer cancel()

	target, ok := s.cfg.Daemons.Lookup(ctx, account)
	if !ok {
		return ""
	}
	return unions.Capability(target.Root)
}
