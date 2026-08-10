package main

import (
	"fmt"
	"os"
	"strings"

	buildxcommands "github.com/docker/buildx/commands"

	// The drivers register themselves, and buildx's own main is where that
	// normally happens. Without them `docker build` answers "no drivers
	// available": the command is present, correctly wired, and cannot build.
	_ "github.com/docker/buildx/driver/docker"
	_ "github.com/docker/buildx/driver/docker-container"
	_ "github.com/docker/buildx/driver/remote"
	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/command/commands"
	"github.com/docker/cli/cli/flags"
	"github.com/spf13/cobra"

	"github.com/lhns/remote-docker/client/internal/proxy"
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
	if invokingDocker() {
		if cfg, err := resolve(); err == nil {
			endpoint := endpointOf(cfg)
			ours := proxy.DockerHost(endpoint)
			set := os.Getenv("DOCKER_HOST")

			// Managed when the endpoint in play is ours -- whether we chose it
			// or DOCKER_HOST names it. An explicitly set DOCKER_HOST used to
			// skip this entirely, which meant pointing it at our OWN endpoint
			// disabled starting a session and noticing a stale one. Anyone who
			// has run `context install` or exported the printed value is in
			// exactly that position, and the integration suite is too, which is
			// how this was found.
			//
			// Make a usable session available: start one if nothing is serving,
			// and replace one built from a different commit if that costs
			// nothing. Requiring `up` first made the embedded CLI -- the thing
			// that exists so nothing has to be installed -- the one part of
			// this tool with a setup step, and a stale session made a freshly
			// updated client behave like the old one.
			//
			// Which workspace this is comes from the same resolution as every
			// other command, so `--workspace ci docker ps` uses ci.
			if set == "" || set == ours {
				ensureDaemon(cfg, endpoint)
			}
			// A DOCKER_HOST naming something else is left alone: it is a
			// deliberate instruction to talk to that, not to us.
			if set == "" {
				_ = os.Setenv("DOCKER_HOST", ours)
			}
		} else if os.Getenv("DOCKER_HOST") == "" {
			_ = os.Setenv("DOCKER_HOST", proxy.DockerHost(""))
		}
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
	installModernBuilder(cmd, dockerCli)
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

// installModernBuilder replaces `build` with buildx's, which is what the real
// docker CLI does when the plugin is present.
//
// Not an extra subcommand. Upstream, `docker build` IS `docker buildx build`
// whenever buildx is installed, and the classic builder is only the fallback
// for when it is not -- so adding a parallel `buildx` and leaving `build` on
// the old path would be a shape docker does not have.
//
// The classic builder is what we shipped until now, and it was not a choice:
// buildx is a separate plugin binary, so embedding docker/cli alone got the
// pre-BuildKit path silently, even with DOCKER_BUILDKIT=1. ADR 0009 recorded
// the opposite and was wrong.
//
// `buildx` is registered too, because it is a real command with subcommands
// docker exposes -- bake, imagetools, du -- and hiding them would be a
// different kind of surprise.
func installModernBuilder(cmd *cobra.Command, dockerCli *command.DockerCli) {
	root := buildxcommands.NewRootCmd("buildx", true, dockerCli)

	for _, sub := range root.Commands() {
		if sub.Name() != "build" {
			continue
		}
		// Off buildx's root before going onto ours: cobra keeps one parent
		// per command, and a command still owned by another tree inherits
		// that tree's flags and help.
		root.RemoveCommand(sub)
		if old, _, err := cmd.Find([]string{"build"}); err == nil {
			cmd.RemoveCommand(old)
		}
		sub.Use = "build [OPTIONS] PATH | URL | -"
		cmd.AddCommand(sub)
		break
	}

	// The buildx ROOT is deliberately not registered. Its subcommands expect
	// the plugin harness to have run -- `docker buildx version` panics on a
	// nil dereference without it -- and `build` is what docker's own tree
	// exposes anyway.
	_ = root
}
