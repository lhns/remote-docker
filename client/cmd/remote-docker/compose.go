package main

// `docker compose`, in this binary.
//
// ADR 0009 could not have this: compose v2 pinned docker/cli back a major
// version and buildx back seven minors, which would have cost BuildKit. That
// record named its own revisit trigger, the moby/moby migration, and compose
// v5 completed it: it builds against the same docker/cli, buildx and buildkit
// this binary already carries, so the two now agree rather than compete.

import (
	"github.com/docker/cli/cli/command"
	composecmd "github.com/docker/compose/v5/cmd/compose"
	"github.com/docker/compose/v5/cmd/prompt"
	"github.com/docker/compose/v5/pkg/compose"
	"github.com/spf13/cobra"
)

// installCompose adds the compose command tree, the way docker's own plugin
// harness would.
//
// Not the harness itself. `plugin.Run` expects to be a separate process that
// docker execs, and it would initialise a second CLI over the one we already
// built and pointed at our endpoint. What it does that matters here is two
// lines, and they are these.
func installCompose(cmd *cobra.Command, dockerCli *command.DockerCli) {
	backend := &composecmd.BackendOptions{
		Options: []compose.Option{
			// The confirmation prompt for destructive operations. Without it
			// `compose down --remove-orphans` has nothing to ask with.
			compose.WithPrompt(prompt.NewPrompt(dockerCli.In(), dockerCli.Out()).Confirm),
		},
	}

	sub := composecmd.RootCommand(dockerCli, backend)
	sub.AddCommand(composecmd.HooksCommand())
	cmd.AddCommand(sub)
}
