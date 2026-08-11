package main

// What happens when a word is not one of our commands.
//
// cobra's answer for a command that only groups subcommands is to print the
// help and exit 0, which is wrong twice over: a typo is indistinguishable from
// a deliberate `--help`, and a script sees success. It hid a whole session of
// mistyped commands on a terminal that was corrupting keystrokes, because
// every one of them produced a cheerful help screen and nothing else.
//
// `workspace creat dev` was worse than silent. Bare `workspace` lists, so the
// misspelled `create` fell back to the parent, LISTED the workspaces and
// exited 0, having created nothing.

import (
	"fmt"

	"github.com/spf13/cobra"
)

// onlySubcommands is the Args rule for a command whose arguments are the names
// of its own subcommands and nothing else.
//
// Paired with a RunE on any command that is not otherwise runnable: cobra
// returns help for an unrunnable command BEFORE it validates arguments, so a
// rule alone would never be reached.
func onlySubcommands(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	// The nearest real command, when there is one. A corrupted or fat-fingered
	// word is the common case here, and naming the intended command is a
	// better answer than a list of thirty.
	//
	// The distance has to be set. cobra defaults it to 0, and SuggestionsFor
	// uses that value as given rather than falling back to 2 the way
	// findSuggestions does, so unset it matches on a common prefix and nothing
	// else: `creat` found `create` and `statuss` found nothing at all.
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
