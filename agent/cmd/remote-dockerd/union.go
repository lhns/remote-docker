package main

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/lhns/remote-docker/core-agent/union"
)

// newUnionCommand is how the agent runs itself again to mount a delegated
// share's union (ADR 0044).
//
// Hidden, and not because it is dangerous: it is unusable by hand. Everything
// it needs arrives in the environment, and the namespace it enters belongs to a
// daemon whose pid only the agent knows. A person typing it gets a refusal
// about RD_UNION_MODE, which is the right answer to a question nobody meant to
// ask.
//
// It exists at all because a mount namespace cannot be entered from inside the
// agent: setns(CLONE_NEWNS) refuses a caller that shares filesystem state, and
// every Go thread does. See core-agent/union.
func newUnionCommand() *cobra.Command {
	return &cobra.Command{
		Use:    union.Command,
		Short:  "Serve one share's union mount (used by the agent on itself)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			spec, mode, err := union.FromEnv(os.Getenv)
			if err != nil {
				return err
			}
			if mode == union.ModeUnmount {
				return union.Release(spec)
			}
			return union.Serve(spec)
		},
	}
}
