package main

import (
	"fmt"
	"os"

	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/command/commands"
	"github.com/docker/cli/cli/flags"
	"github.com/spf13/cobra"

	"github.com/lhns/remote-docker/internal/client/proxy"
)

// newDockerCommand mounts the real Docker CLI's command tree under
// `remote-docker docker ...`.
//
// The premise of the project is a machine that cannot have software installed,
// and the docker CLI is software. Solving the daemon and the filesystem while
// still requiring a local Docker installation would leave the original problem
// half solved (ADR 0009).
//
// This is the genuine command tree, not a reimplementation: every subcommand
// the real CLI has, with its real flags and its real help.
func newDockerCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "docker",
		Short: "Run any docker command against the workspace",
		Long: `The complete Docker CLI, talking to the workspace daemon.

Needs a session: run "remote-docker up" in another terminal. This command
finds that session's endpoint on its own, so DOCKER_HOST does not have to be
set -- though an explicit one is respected.`,
		SilenceUsage:  true,
		SilenceErrors: true,

		// The client options below go on Flags() rather than PersistentFlags(),
		// which is what the real CLI does and is not a style choice: cobra
		// merges persistent flags into every subcommand, and `--context` has
		// the shorthand -c, which `build` already uses for --cpu-shares.
		// Installing them persistently panics on `docker build --help`.
		// TraverseChildren is what still lets `docker --context x ps` parse.
		TraverseChildren: true,
	}

	// Point the embedded client at our endpoint unless the user has already
	// chosen one. Without this, the CLI would look for a local daemon that by
	// premise is not installed.
	if os.Getenv("DOCKER_HOST") == "" {
		endpoint := ""
		if cfg, err := resolve(); err == nil {
			endpoint = cfg.EndpointFor(proxy.DefaultEndpoint)
		}
		_ = os.Setenv("DOCKER_HOST", proxy.DockerHost(endpoint))
	}

	dockerCli, err := command.NewDockerCli()
	if err != nil {
		// Reported when the command runs rather than at construction, so a
		// failure here cannot stop `remote-docker --help` working.
		cmd.RunE = func(*cobra.Command, []string) error {
			return fmt.Errorf("initialising the docker client: %w", err)
		}
		return cmd
	}

	opts := flags.NewClientOptions()
	opts.InstallFlags(cmd.Flags())
	opts.SetDefaultOptions(cmd.Flags())

	if err := dockerCli.Initialize(opts); err != nil {
		cmd.RunE = func(*cobra.Command, []string) error {
			return fmt.Errorf("initialising the docker client: %w", err)
		}
		return cmd
	}

	commands.AddCommands(cmd, dockerCli)
	return cmd
}
