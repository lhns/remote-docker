package main

import (
	"fmt"
	"os"
	"strings"

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
	//
	// Gated on the invocation actually BEING a docker command, because this
	// function runs while the root command is built -- so `remote-docker gc`,
	// and even `--help`, used to probe the endpoint and could open a whole
	// file-serving session that then raced the real command's own session
	// inside one process.
	if invokingDocker() && os.Getenv("DOCKER_HOST") == "" {
		endpoint := ""
		if cfg, err := resolve(); err == nil {
			endpoint = cfg.EndpointFor(proxy.DefaultEndpoint)

			// Make a usable session available: start one if nothing is
			// serving, and replace one built from a different commit if that
			// costs nothing. Requiring `up` first made the embedded CLI -- the
			// thing that exists so nothing has to be installed -- the one part
			// of this tool with a setup step, and a stale daemon made a freshly
			// updated client behave like the old one.
			//
			// Which workspace this is comes from the same resolution as every
			// other command, so `--workspace ci docker ps` uses ci.
			ensureDaemon(cfg, endpoint)
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

// invokingDocker reports whether this process was asked to run a docker
// command, by finding the first argument that is not a flag or a flag's value.
//
// Crude on purpose. Cobra has not parsed anything yet -- it is still being
// assembled -- so there is nothing better to ask, and being wrong costs only
// a session that is opened slightly too eagerly or not eagerly enough.
func invokingDocker() bool {
	// Every root flag takes a value, so a flag consumes the token after it
	// unless it was written as --flag=value.
	takesValue := map[string]bool{
		"--workspace": true, "--host": true, "--port": true,
		"--user": true, "--endpoint": true,
	}
	skip := false
	for _, arg := range os.Args[1:] {
		if skip {
			skip = false
			continue
		}
		if strings.HasPrefix(arg, "-") {
			if name, _, hasValue := strings.Cut(arg, "="); !hasValue && takesValue[name] {
				skip = true
			}
			continue
		}
		return arg == "docker"
	}
	return false
}
