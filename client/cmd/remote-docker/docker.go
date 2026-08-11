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
	"github.com/docker/cli/cli"
	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/command/commands"
	"github.com/spf13/cobra"

	"github.com/lhns/remote-docker/client/internal/proxy"
)

// newDockerCommand builds the real Docker CLI's command tree, which is this
// program's root.
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
		Use:   programName(),
		Short: "Docker on a remote workspace, with your local files really mounted",
		Long: `The complete Docker CLI, talking to a remote workspace's daemon, with your
own directories really mounted into the containers. Not copied, not synced, so
bind mounts, published ports and the standard tooling behave the way they would
locally.

It finds the session's endpoint itself and starts a session if none is
running, so there is nothing to do first. An explicit DOCKER_HOST is respected.

Nothing needs to be installed on this machine beyond this binary. Rename it to
"docker" and every command below is spelled the way it is everywhere else.

"remote" is where this program's own commands live.`,
		SilenceUsage:  true,
		SilenceErrors: true,

		// So `docker --version` and `docker -v` answer, which is the first
		// thing anybody types at a docker they are not sure about. Cobra adds
		// the flag because this field is set, and the shorthand because
		// nothing at this level uses -v, which matches the real CLI, where
		// -v is version at the root and --volume only under `run`.
		Version: dockerVersionLine(),

		// Cobra decides traversal HERE and for the whole tree: ExecuteC calls
		// Find() unless the root sets this, and Find parses every flag at the
		// deepest command it lands on. So `docker --context dev ps` handed
		// --context to `ps`, which has never heard of it, and `compose -f x up`
		// handed -f to `up`. Both are flags of a command halfway down.
		TraverseChildren: true,

		// A word that is not a command is an error, not a help screen. See
		// unknown.go: the RunE is what makes the rule reachable at all.
		Args: onlySubcommands,
		RunE: helpWhenBare,
	}
	// docker's own root setup: the real help layout, with its Common,
	// Management and Commands sections, its flag error format and exit code,
	// and the client options installed on Flags() rather than
	// PersistentFlags(). The last part is not a style choice -- `--context`
	// has the shorthand -c, which `build` already uses for --cpu-shares, so
	// installing them persistently panics on `docker build --help`.
	//
	// Hand-rolled here until now, which meant cobra's default help: one flat
	// list of sixty commands where docker's own groups them.
	opts, _ := cli.SetupRootCommand(cmd)

	if invokingDocker() {
		pointAtOurEndpoint()
	}

	// After SetupRootCommand, which sets its own. Both would render
	// "Docker version Docker version 29.7.2, build ...": the line we build is
	// already the whole answer.
	cmd.SetVersionTemplate("{{.Version}}\n")

	dockerCli, err := command.NewDockerCli()
	if err != nil {
		// Reported when the command runs rather than at construction, so a
		// failure here cannot stop `remote-docker --help` working.
		cmd.RunE = func(*cobra.Command, []string) error {
			return fmt.Errorf("initialising the docker client: %w", err)
		}
		return cmd
	}

	opts.SetDefaultOptions(cmd.Flags())

	if err := dockerCli.Initialize(opts); err != nil {
		cmd.RunE = func(*cobra.Command, []string) error {
			return fmt.Errorf("initialising the docker client: %w", err)
		}
		return cmd
	}

	// After Initialize, because that is what loads the config file this reads.
	credentials := checkCredentialHelpers(dockerCli.ConfigFile(), lookPath, os.Stderr)

	commands.AddCommands(cmd, dockerCli)
	installModernBuilder(cmd, dockerCli)
	installCompose(cmd, dockerCli)

	// Raised when a command RUNS, not by leaving commands out of the tree.
	// Returning early here built a docker with no subcommands, so `docker run
	// --rm` failed with "unknown flag: --rm", which is a worse lie than the
	// one it was replacing.
	//
	// PersistentPreRunE, so it reaches every subcommand. Cobra runs the
	// closest one up the chain: compose chains to its parent's, `docker stack`
	// replaces it and is the one place this will not fire, which is Swarm and
	// untested anyway (ADR 0009). Help is unaffected, because cobra answers
	// --help before it runs hooks.
	if credentials != nil {
		cmd.PersistentPreRunE = func(*cobra.Command, []string) error { return credentials }
	}
	return cmd
}

// pointAtOurEndpoint aims the embedded CLI at this workspace, and makes a
// session available to answer.
//
// Called only when the invocation is actually a docker command, because the
// tree is built for every command: `remote gc`, and even `--help`, used to
// probe the endpoint and could open a whole file-serving session that then
// raced the real command's own, inside one process.
func pointAtOurEndpoint() {
	cfg, err := resolve()
	if err != nil {
		// No workspace resolved, so there is nothing to aim at beyond the
		// default endpoint.
		if os.Getenv("DOCKER_HOST") == "" {
			_ = os.Setenv("DOCKER_HOST", proxy.DockerHost(""))
		}
		return
	}

	endpoint := endpointOf(cfg)
	ours := proxy.DockerHost(endpoint)
	set := os.Getenv("DOCKER_HOST")

	// Managed whenever the endpoint in play is ours, whether we chose it or
	// DOCKER_HOST names it. Skipping this when DOCKER_HOST was set meant that
	// pointing it at our OWN endpoint, which is what the printed value and the
	// docker context both do, disabled starting a session and noticing a stale
	// one.
	//
	// Start one if nothing is serving, and replace one built from a different
	// commit when that costs nothing. Requiring `start` first would give the
	// embedded CLI, which exists so that nothing has to be installed, a setup
	// step of its own.
	//
	// The workspace comes from the same resolution as every other command.
	if set == "" || set == ours {
		ensureDaemon(cfg, endpoint)
	}
	// A DOCKER_HOST naming something else is left alone: it is a deliberate
	// instruction to talk to that, not to us.
	if set == "" {
		_ = os.Setenv("DOCKER_HOST", ours)
	}
}

// NoSessionEnv tells a docker command not to make a session available.
//
// Set on the docker commands this program runs itself. This binary may BE the
// `docker` on PATH, so `exec.LookPath("docker")` finds us, and `remote create`
// writing a docker context would spawn us to start a whole file-serving
// session in order to write a line of JSON.
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

	// Some docker commands never reach a daemon, and one of them is how we
	// invoke ourselves. Starting a session for `docker context create` is not
	// wrong so much as absurd: it opens an SSH connection, an NFS server and a
	// reverse tunnel in order to write a file on this machine.
	//
	// No subcommand at all (`docker`, `--help`, `--version`) is the same answer
	// for the same reason. Printing help used to be enough to start a session.
	verb, ok := firstArgument(os.Args[1:])
	if !ok {
		return false
	}
	switch verb {
	case "remote", "context", "completion", "help":
		return false
	}
	return true
}

// valuedRootFlags are the docker root flags that consume the argument after
// them, which is what lets a scan tell a flag's VALUE from a subcommand.
//
// `docker --context remote ps` is the case that makes this necessary rather
// than tidy: without it, a context somebody named "remote" reads as our own
// namespace and the docker command silently runs with no session.
var valuedRootFlags = map[string]bool{
	"--config":  true,
	"--context": true, "-c": true,
	"--host": true, "-H": true,
	"--log-level": true, "-l": true,
	"--tlscacert": true, "--tlscert": true, "--tlskey": true,
}

// firstArgument returns the first argument that is not a flag or a flag's
// value, which is the subcommand.
func firstArgument(args []string) (string, bool) {
	skip := false
	for _, arg := range args {
		if skip {
			skip = false
			continue
		}
		if strings.HasPrefix(arg, "-") {
			// --flag=value carries its own value, so nothing follows it.
			if name, _, hasValue := strings.Cut(arg, "="); !hasValue && valuedRootFlags[name] {
				skip = true
			}
			continue
		}
		return arg, true
	}
	return "", false
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
