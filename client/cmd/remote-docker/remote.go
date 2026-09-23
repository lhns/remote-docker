package main

// Everything that is ours, under `remote` (ADR 0024). A remote is a workspace,
// so `ls`, `create` and the rest sit directly under it.

import (
	"context"
	"fmt"

	"github.com/lhns/remote-docker/client/internal/config"
	"github.com/lhns/remote-docker/client/internal/session"
	"github.com/lhns/remote-docker/core-client/keys"
	"github.com/spf13/cobra"
)

// overrides collects the flags below, which every `remote` command resolves
// through.
var overrides config.Overrides

func newRemoteCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "remote",
		Aliases: []string{"remotes"},
		Short:   "Manage remote workspaces and this machine's session",
		Long: `The workspaces this machine knows about, and the session that reaches them.

Everything else in this program is the Docker CLI itself, talking to whichever
workspace the current docker context names.`,
		Args: onlySubcommands,
		RunE: helpWhenBare,
	}

	// Never on the root, which has docker's own --host and --user (ADR 0024).
	// A docker command's workspace is chosen by the docker context instead.
	cmd.PersistentFlags().StringVar(&overrides.Workspace, "workspace", "", "which configured workspace to use")
	cmd.PersistentFlags().StringVar(&overrides.Host, "host", "", "workspace address")
	cmd.PersistentFlags().IntVar(&overrides.Port, "port", 0, "workspace SSH port")
	cmd.PersistentFlags().StringVar(&overrides.User, "user", "", "workspace account")
	cmd.PersistentFlags().StringVar(&overrides.Endpoint, "endpoint", "", "local Docker endpoint to serve")

	cmd.AddCommand(
		// The remotes themselves.
		newWorkspaceCreateCommand(),
		newWorkspaceListCommand(),
		newWorkspaceRemoveCommand(),
		newWorkspaceUseCommand(),
		newWorkspaceInspectCommand(),

		// This machine's session to one of them.
		newStatusCommand(),
		newStartCommand(),
		newStopCommand(),
		newRestartCommand(),

		// The rest.
		newMachineCommand(),
		newEnrollCommand(),
		newGCCommand(),
		newVersionCommand(),
	)
	return cmd
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

func newEnrollCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "enroll",
		Short: "Print the public key to hand to whoever runs the workspace",
		Long: `Generates this machine's keypair on first use and prints the public half.

Enrolment is out of band: someone with access to the workspace saves the key
as authorized_keys.d/<your account>.pub, and the filename becomes your unix
account there.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}

			// The comment is what attributes a key to a machine in
			// authorized_keys.d.
			key, err := keys.LoadOrCreateKey(config.KeyPath(), config.KeyComment())
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintln(out, "Give this to whoever runs the workspace.")
			_, _ = fmt.Fprintf(out, "It must be saved as: authorized_keys.d/%s.pub\n", cfg.User)
			_, _ = fmt.Fprintln(out, "(the filename becomes your account name there)")
			_, _ = fmt.Fprintln(out)
			// Again: a key loaded from disk carries no comment of its own.
			_, _ = fmt.Fprintln(out, key.AuthorizedKey(config.KeyComment()))
			return nil
		},
	}
}

func newGCCommand() *cobra.Command {
	var orphans bool

	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Remove share volumes this account is no longer using",
		Long: `Each directory bound into a container gets a volume on the workspace, and
they outlive the containers that used them.

Only volumes this machine created, for this account, that no container refers
to. A volume you created yourself is never touched, whatever it is named, and
neither is the one for the directory this runs in.

Volumes another of your machines created are left alone, because this one
cannot tell whether that machine is still using them. --orphans additionally
removes those that name no machine at all, which are the ones left by a version
before machines were named, or by this machine before its key was replaced.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withQuerySession(func(ctx context.Context, s *session.Session) error {
				removed, err := s.Collect(ctx, session.CollectOptions{Orphans: orphans})
				if err != nil {
					return err
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "removed %d unused share volume(s)\n", removed)
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&orphans, "orphans", false,
		"also remove unused share volumes that name no machine")
	return cmd
}
