// Package dockercli runs the docker CLI, which is how the agent talks to a
// daemon at all.
//
// There is no Go Docker client here on purpose: adding one to ask "what is
// this volume's mountpoint" or "what is this container's pid" would be a large
// dependency for a `--format` string, and the image already carries the CLI.
// That trade is recorded in internal/server/elevate, which made it first.
//
// This package exists because four callers then made it separately, and each
// re-decided the same four things: which binary, how to name the daemon, how
// to trim the output, and how to wrap the error. Two of them spelled the host
// flag differently (`--host` in one, `-H` in another) and the fourth
// hardcoded the binary with no seam at all, which is why the agent's version
// lookup could not be tested.
package dockercli

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// CLI runs docker commands against one daemon.
//
// The zero value talks to the daemon on the default socket, which is the
// workspace's own -- the parent of every per-account daemon (ADR 0019).
type CLI struct {
	// Host is the daemon to talk to, as a DOCKER_HOST-style value such as
	// "unix:///run/rd/alice/docker.sock". Empty means the default socket.
	Host string
}

// binary is not configurable, and that is a deliberate narrowing.
//
// Three separate "empty means docker on PATH" fields existed across the agent
// and no caller ever set any of them, so each was a knob that only ever had
// one setting -- while making every call site carry a branch to honour it.
const binary = "docker"

// Cmd builds a command against this daemon, for callers that need to own the
// process -- streaming its output, forwarding signals to it.
func (c CLI) Cmd(ctx context.Context, args ...string) *exec.Cmd {
	if c.Host != "" {
		args = append([]string{"--host", c.Host}, args...)
	}
	return exec.CommandContext(ctx, binary, args...)
}

// Line runs a command and returns its trimmed stdout.
//
// For the `--format` queries that make up most of what the agent asks: a pid,
// a status, a mountpoint. stderr is left to the caller's error handling.
func (c CLI) Line(ctx context.Context, args ...string) (string, error) {
	out, err := c.Cmd(ctx, args...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Run performs a command for its effect, and includes docker's own output in
// the error.
//
// what names the operation in the failure -- "starting alice's daemon" rather
// than "exit status 125". CombinedOutput, not Run: an exit status alone says
// something failed without saying why, and docker's message is the whole
// diagnosis.
func (c CLI) Run(ctx context.Context, what string, args ...string) error {
	out, err := c.Cmd(ctx, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", what, err, strings.TrimSpace(string(out)))
	}
	return nil
}
