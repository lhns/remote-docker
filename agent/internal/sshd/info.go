package sshd

import (
	"context"
	"time"

	"github.com/lhns/remote-docker/agent/internal/dockercli"
	"github.com/lhns/remote-docker/agent/internal/unions"
	"github.com/lhns/remote-docker/core/workspace"
)

// infoQueryTimeout bounds each query: a daemon slow enough to sit on `docker
// info` is exactly the daemon whose client should not be blocked introducing
// itself.
const infoQueryTimeout = 5 * time.Second

// infoField asks the account's daemon one --format question, and answers
// fallback when the daemon is not up, or does not answer in time.
//
// Lookup, never Ensure: workspace-info is the client's first round trip, right
// after Warm fired at authentication, so the daemon may still be booting. The
// answers are displayed, and the next command starts the daemon. Ensure here
// fails silently, as every first connection after a restart waiting on a cold
// dind for a version string. routing_test.go pins it.
func (s *Server) infoField(ctx context.Context, account, fallback string, args ...string) string {
	ctx, cancel := context.WithTimeout(ctx, infoQueryTimeout)
	defer cancel()

	target, ok := s.cfg.Daemons.Lookup(ctx, account)
	if !ok {
		return fallback
	}
	out, err := s.line(ctx, target.Host, args...)
	if err != nil {
		return fallback
	}
	return out
}

// line asks one daemon one question, through query when a test set it.
func (s *Server) line(ctx context.Context, host string, args ...string) (string, error) {
	if s.query != nil {
		return s.query(ctx, host, args...)
	}
	return dockercli.CLI{Host: host}.Line(ctx, args...)
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
