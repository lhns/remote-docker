// Command remote-dockerd runs inside the workspace container.
//
// Today it does one job: elevate itself to a privileged container under Docker
// Swarm, which cannot run privileged tasks. It will grow into the workspace
// agent proper, replacing sshd, the key watcher and the mount helpers
// (docs/adr/0010).
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/lhns/remote-docker/internal/server/elevate"
)

// version is set at build time by the release workflow.
var version = "dev"

func main() {
	if err := newRootCommand().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "remote-dockerd:", err)
		os.Exit(1)
	}
}

func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:           "remote-dockerd",
		Short:         "The remote-docker workspace agent",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newServeCommand(), newElevateCommand(), newDaemonsCommand(), newVersionCommand())
	return root
}

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), version)
			return nil
		},
	}
}

func newElevateCommand() *cobra.Command {
	var hostSocket string

	cmd := &cobra.Command{
		Use:   "elevate",
		Short: "Relaunch this container privileged, outside Swarm",
		Long: `Swarm cannot run privileged tasks. This inspects the current container
through the host's Docker socket and starts a privileged copy of itself that
shares this task's network namespace -- so the port Swarm publishes reaches
the real workspace inside it.

The host's Docker socket is deliberately NOT passed to the privileged copy.

Signals are forwarded, so stopping the Swarm task stops the workspace. Without
that the privileged container would keep running and keep the published port,
and the replacement task would fail to bind it.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Deliberately NOT a signal-cancelled context. The runner forwards
			// signals to the child so it can stop gracefully; cancelling the
			// context here would kill `docker run` outright and defeat that.
			ctx := context.Background()

			runner := &elevate.Runner{
				HostSocket: hostSocket,
				Log: func(format string, args ...any) {
					_, _ = fmt.Fprintf(os.Stderr, "[elevate] "+format+"\n", args...)
				},
			}

			// The child's exit code is ours: a supervisor watching this task
			// should see what the workspace actually did.
			code, err := runner.Run(ctx)
			if err != nil {
				return err
			}
			if code != 0 {
				os.Exit(code)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&hostSocket, "host-socket", "",
		"where the host's Docker socket is mounted (default "+elevate.DefaultHostSocket+")")
	return cmd
}
