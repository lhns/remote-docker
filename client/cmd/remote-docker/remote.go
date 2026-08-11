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
	"github.com/spf13/cobra"
)

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
		newUpCommand(),

		// The rest.
		newEnrollCommand(),
		newGCCommand(),
		newVersionCommand(),
	)
	return cmd
}
