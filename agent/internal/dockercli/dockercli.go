// Package dockercli runs the docker CLI, which is how the agent talks to a
// daemon at all: a Go Docker client would be a large dependency for a
// `--format` string, and the image carries the CLI already.
package dockercli

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// CLI runs docker commands against one daemon. The zero value is the default
// socket: the workspace's own daemon, parent of every per-account one.
type CLI struct {
	// Host is the daemon to talk to, as a DOCKER_HOST-style value such as
	// "unix:///run/rd/alice/docker.sock". Empty means the default socket.
	Host string
}

const binary = "docker"

// ServerVersionArgs asks a daemon for its version, which is the one request
// that says a daemon ANSWERS: a socket file alone is what a daemon that died
// during startup leaves behind.
func ServerVersionArgs() []string {
	return []string{"version", "--format", "{{.Server.Version}}"}
}

// Cmd builds a command against this daemon, for callers that need to own the
// process: streaming its output, forwarding signals to it.
func (c CLI) Cmd(ctx context.Context, args ...string) *exec.Cmd {
	if c.Host != "" {
		args = append([]string{"--host", c.Host}, args...)
	}
	return exec.CommandContext(ctx, binary, args...)
}

// Line runs a command and returns its trimmed stdout, for `--format` queries.
func (c CLI) Line(ctx context.Context, args ...string) (string, error) {
	out, err := c.Cmd(ctx, args...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Run performs a command for its effect. The error carries what, naming the
// operation, and docker's own output, since an exit status alone says nothing.
func (c CLI) Run(ctx context.Context, what string, args ...string) error {
	out, err := c.Cmd(ctx, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", what, err, strings.TrimSpace(string(out)))
	}
	return nil
}
