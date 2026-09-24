package main

// An unknown subcommand is an error. cobra would print help and exit 0, and a
// parent that runs on its own (`remote` lists workspaces) turned `remote creat
// dev` into a listing that exited 0 having created nothing.

import (
	"fmt"

	"github.com/spf13/cobra"
)

// onlySubcommands is the Args rule for a command that takes only subcommand
// names. An unrunnable command also needs a RunE (helpWhenBare): cobra returns
// help for it before validating arguments, so this rule would never run.
func onlySubcommands(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	// cobra defaults the distance to 0 and SuggestionsFor does not fall back to
	// 2, so unset it matches prefixes only: `statuss` found nothing.
	if cmd.SuggestionsMinimumDistance <= 0 {
		cmd.SuggestionsMinimumDistance = 2
	}
	if near := cmd.SuggestionsFor(args[0]); len(near) > 0 {
		return fmt.Errorf("%q is not a %s command\n  fix: did you mean `%s %s`?",
			args[0], cmd.CommandPath(), cmd.CommandPath(), near[0])
	}
	return fmt.Errorf("%q is not a %s command\n  fix: `%s --help` lists them",
		args[0], cmd.CommandPath(), cmd.CommandPath())
}

// helpWhenBare is the RunE for a command that does nothing on its own.
func helpWhenBare(cmd *cobra.Command, _ []string) error { return cmd.Help() }
