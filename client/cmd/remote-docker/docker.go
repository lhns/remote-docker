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
	nameTheEmbeddedCLI()

	cmd := &cobra.Command{
		Use:   "docker",
		Short: "Run any docker command against the workspace",
		Long: `The complete Docker CLI, talking to the workspace daemon.

It finds the session's endpoint itself and starts a session if none is
running, so there is nothing to do first. An explicit DOCKER_HOST is respected.

"docker compose" is included: compose v5 builds against the same docker/cli,
buildx and buildkit this binary carries.`,
		SilenceUsage:  true,
		SilenceErrors: true,

		// So `docker --version` and `docker -v` answer, which is the first
		// thing anybody types at a docker they are not sure about. Cobra adds
		// the flag because this field is set, and the shorthand because
		// nothing at this level uses -v, which matches the real CLI, where
		// -v is version at the root and --volume only under `run`.
		Version: dockerVersionLine(),

		// The client options below go on Flags() rather than PersistentFlags(),
		// which is what the real CLI does and is not a style choice: cobra
		// merges persistent flags into every subcommand, and `--context` has
		// the shorthand -c, which `build` already uses for --cpu-shares.
		// Installing them persistently panics on `docker build --help`.
		// TraverseChildren is what still lets `docker --context x ps` parse.
		TraverseChildren: true,
	}
	// Cobra's default template would render "docker version Docker version
	// 29.7.2, build ...". The line is already the whole answer.
	cmd.SetVersionTemplate("{{.Version}}\n")

	// Point the embedded client at our endpoint unless the user has already
	// chosen one. Without this, the CLI would look for a local daemon that by
	// premise is not installed.
	//
	// Gated on the invocation actually BEING a docker command, because this
	// function runs while the root command is built, so `remote-docker gc`,
	// and even `--help`, used to probe the endpoint and could open a whole
	// file-serving session that then raced the real command's own session
	// inside one process.
	if invokingDocker() {
		if cfg, err := resolve(); err == nil {
			endpoint := endpointOf(cfg)
			ours := proxy.DockerHost(endpoint)
			set := os.Getenv("DOCKER_HOST")

			// Managed whenever the endpoint in play is ours, whether we chose
			// it or DOCKER_HOST names it. Skipping this when DOCKER_HOST was
			// set meant that pointing it at our OWN endpoint, which is what
			// the printed value and the docker context both do, disabled
			// starting a session and noticing a stale one.
			//
			// Start one if nothing is serving, and replace one built from a
			// different commit when that costs nothing. Requiring `start`
			// first would give the embedded CLI, which exists so that nothing
			// has to be installed, a setup step of its own.
			//
			// The workspace comes from the same resolution as every other
			// command, so `--workspace ci docker ps` uses ci.
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

	// After Initialize, because that is what loads the config file this reads.
	dropMissingCredentialHelpers(dockerCli.ConfigFile(), lookPath, os.Stderr)

	commands.AddCommands(cmd, dockerCli)
	installModernBuilder(cmd, dockerCli)
	installCompose(cmd, dockerCli)
	return cmd
}

// NoSessionEnv tells a docker command not to make a session available.
//
// Set on the docker commands this program runs itself. Once `docker` on PATH
// IS this binary, `exec.LookPath("docker")` finds us, so `workspace create`
// writing a docker context would spawn us, and we would start a whole
// file-serving session in order to write a line of JSON. A docker command we
// run ourselves must not start a session.
const NoSessionEnv = "REMOTE_DOCKER_NO_SESSION"

// invokingDocker reports whether this process was asked to run a docker
// command that should have a session made available for it.
//
// Crude on purpose. Cobra has not parsed anything yet, it is still being
// assembled, so there is nothing better to ask, and being wrong costs only
// a session that is opened slightly too eagerly or not eagerly enough.
func invokingDocker() bool {
	if os.Getenv(NoSessionEnv) != "" {
		return false
	}

	args := os.Args[1:]
	if !invokedAsDocker() {
		// Under our own name the docker tree is a subcommand, so the first
		// non-flag argument has to BE "docker". Under the alias every argument
		// is already docker's and the same scan asks a different question:
		// which docker subcommand it is.
		var found bool
		if args, found = afterDockerVerb(args); !found {
			return false
		}
	}

	// Some docker commands never reach a daemon, and one of them is how we
	// invoke ourselves. Starting a session for `docker context create` is not
	// wrong so much as absurd: it opens an SSH connection, an NFS server and a
	// reverse tunnel in order to write a file on this machine.
	//
	// No subcommand at all (`docker`, `docker --help`, `docker --version`) is
	// the same answer for the same reason. Printing help used to be enough to
	// start a session.
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		switch arg {
		case "context", "completion", "help":
			return false
		}
		return true
	}
	return false
}

// afterDockerVerb finds the "docker" subcommand among our own arguments and
// returns what follows it.
func afterDockerVerb(args []string) ([]string, bool) {
	// Every root flag takes a value, so a flag consumes the token after it
	// unless it was written as --flag=value.
	takesValue := map[string]bool{
		"--workspace": true, "--host": true, "--port": true,
		"--user": true, "--endpoint": true,
	}
	skip := false
	for i, arg := range args {
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
		if arg != dockerName {
			return nil, false
		}
		return args[i+1:], true
	}
	return nil, false
}

// installModernBuilder replaces `build` with buildx's, which is what the real
// docker CLI does when the plugin is present.
//
// Not an extra subcommand. Upstream, `docker build` IS `docker buildx build`
// whenever buildx is installed, and the classic builder is only the fallback
// for when it is not, so adding a parallel `buildx` and leaving `build` on
// the old path would be a shape docker does not have.
//
// The classic builder is what we shipped until now, and it was not a choice:
// buildx is a separate plugin binary, so embedding docker/cli alone got the
// pre-BuildKit path silently, even with DOCKER_BUILDKIT=1. ADR 0009 recorded
// the opposite and was wrong.
//
// `buildx` is registered too, because it is a real command with subcommands
// docker exposes (bake, imagetools, du) and hiding them would be a
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
	// the plugin harness to have run, and `docker buildx version` panics on a
	// nil dereference without it, and `build` is what docker's own tree
	// exposes anyway.
	_ = root
}
