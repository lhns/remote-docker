package main

// `docker compose`, embedded (ADR 0009).

import (
	"github.com/docker/cli/cli/command"
	composecmd "github.com/docker/compose/v5/cmd/compose"
	"github.com/docker/compose/v5/cmd/prompt"
	"github.com/docker/compose/v5/pkg/compose"
	"github.com/spf13/cobra"
)

// installCompose adds the compose tree as docker's plugin harness would.
// Not plugin.Run itself, which would initialise a second CLI over ours.
func installCompose(cmd *cobra.Command, dockerCli *command.DockerCli) {
	backend := &composecmd.BackendOptions{
		Options: []compose.Option{
			// Without it `compose down --remove-orphans` cannot confirm.
			compose.WithPrompt(prompt.NewPrompt(dockerCli.In(), dockerCli.Out()).Confirm),
		},
	}

	sub := composecmd.RootCommand(dockerCli, backend)
	sub.AddCommand(composecmd.HooksCommand())
	cmd.AddCommand(sub)
}
