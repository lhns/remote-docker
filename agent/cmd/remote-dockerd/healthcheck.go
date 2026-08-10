package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/lhns/remote-docker/agent/internal/dockercli"
)

// healthTimeout bounds the whole check.
//
// A healthcheck that hangs is worse than one that fails: Docker treats a
// timeout as unhealthy anyway, but only after ITS timeout, so a slow check
// delays the verdict rather than giving one.
const healthTimeout = 5 * time.Second

// newHealthcheckCommand answers "is this workspace usable" from inside it.
//
// The deployments used `nc -z 127.0.0.1 2222`, which proves a port is open and
// nothing else. A workspace whose dockerd had died kept passing it, which is
// precisely the case a healthcheck exists to catch -- the agent is fine, the
// port is fine, and every client command fails.
//
// What it checks depends on what it can SEE, and that is deliberate rather
// than lax. Under Swarm the healthcheck runs in the unprivileged task, which
// shares the privileged child's network namespace but not its filesystem: the
// SSH port is reachable, the daemon's socket is not. Failing on a socket this
// container was never going to have would report the topology as broken.
func newHealthcheckCommand() *cobra.Command {
	var addr, socket string

	cmd := &cobra.Command{
		Use:   "healthcheck",
		Short: "Report whether this workspace is serving",
		Long: `Exits 0 when the workspace is usable and non-zero with a reason when it is not.

Checks that SSH is accepting, and that the workspace's own Docker daemon
answers when its socket is visible from here. Intended for a container
healthcheck:

    test: ["CMD", "remote-dockerd", "healthcheck"]`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), healthTimeout)
			defer cancel()

			if err := sshAccepting(ctx, addr); err != nil {
				return err
			}

			// Skipped rather than failed when it is not there: see above.
			if _, err := os.Stat(socket); err != nil {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "ok (ssh; no local docker socket to check)")
				return nil
			}
			if err := dockerAnswering(ctx); err != nil {
				return err
			}

			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "ok (ssh, docker)")
			return nil
		},
	}

	cmd.Flags().StringVar(&addr, "addr", ":2222", "the address the agent serves SSH on")
	cmd.Flags().StringVar(&socket, "docker-socket", "/var/run/docker.sock",
		"the workspace's own Docker socket; skipped when absent")
	return cmd
}

// sshAccepting dials the agent's own port.
//
// Loopback rather than the bind address: the check runs inside, and a bind of
// ":2222" is not an address anything can connect to.
func sshAccepting(ctx context.Context, addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("unreadable address %q: %w", addr, err)
	}

	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		return fmt.Errorf("ssh is not accepting on port %s: %w", port, err)
	}
	return conn.Close()
}

// dockerAnswering asks the daemon for its version.
//
// A request rather than a socket file, because a socket file is what a daemon
// that died during startup leaves behind -- the same distinction that made
// per-account daemons look ready when they were not.
func dockerAnswering(ctx context.Context) error {
	if _, err := (dockercli.CLI{}).Line(ctx, "version", "--format", "{{.Server.Version}}"); err != nil {
		return fmt.Errorf("the workspace's docker daemon is not answering: %w", err)
	}
	return nil
}
