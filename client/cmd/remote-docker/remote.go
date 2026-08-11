package main

// Everything that is ours, under one command.
//
// The root of this binary is the Docker CLI, so that `docker run` is what a
// person types and nothing has to be installed to make that true. That leaves
// exactly one place for the commands that are about the remote itself, and
// this is it.
//
// A remote IS the workspace, so there is no `workspace` level under this: `ls`
// lists them, `create` adds one. The verbs are docker's own, which is the same
// borrowing `workspace` did when it had them, for the same reason.

import (
	"context"
	"fmt"

	"github.com/lhns/remote-docker/client/internal/config"
	"github.com/lhns/remote-docker/client/internal/session"
	"github.com/lhns/remote-docker/client/internal/sshx"
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

	// The workspace options live here rather than on the root, and that is
	// load bearing rather than tidy. The root is the Docker CLI, whose own
	// root flags include --host and --user; a persistent flag of ours with
	// those names would be merged into every docker subcommand, where pflag
	// silently skips the duplicate and the meaning depends on which command
	// you happened to reach. A clashing SHORTHAND is worse still and panics
	// the subtree outright.
	//
	// Which workspace a DOCKER command talks to is a different question with a
	// different answer: the docker context, which is docker's own mechanism
	// and which `remote use` selects.
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

			// config.KeyComment(), not a bare "remote-docker": the comment is the
			// only thing distinguishing one .pub from another in the
			// workspace's authorized_keys.d, and the person adding it needs
			// to know whose machine it came from. LoadOrCreateKey only sets a
			// comment when it GENERATES, and enroll is what usually generates,
			// so this is the spelling that ends up on almost every key.
			key, err := sshx.LoadOrCreateKey(config.KeyPath(), config.KeyComment())
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintln(out, "Give this to whoever runs the workspace.")
			_, _ = fmt.Fprintf(out, "It must be saved as: authorized_keys.d/%s.pub\n", cfg.User)
			_, _ = fmt.Fprintln(out, "(the filename becomes your account name there)")
			_, _ = fmt.Fprintln(out)
			// With the comment, not without. It is the only thing telling
			// whoever files this .pub which machine it came from, and an
			// authorized_keys.d full of anonymous keys cannot be audited.
			_, _ = fmt.Fprintln(out, key.AuthorizedKey(config.KeyComment()))
			return nil
		},
	}
}

// withQuerySession opens a session that only ASKS the workspace things, runs
// fn against it, and closes it.
//

func newGCCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "gc",
		Short: "Remove share volumes this account is no longer using",
		Long: `Each directory bound into a container gets a volume on the workspace, and
they outlive the containers that used them.

Only volumes this client created, for this account, that no container refers
to. A volume you created yourself is never touched, whatever it is named, and
neither is the one for the directory this runs in.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withQuerySession(func(ctx context.Context, s *session.Session) error {
				removed, err := s.Collect(ctx)
				if err != nil {
					return err
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "removed %d unused share volume(s)\n", removed)
				return nil
			})
		},
	}
}
