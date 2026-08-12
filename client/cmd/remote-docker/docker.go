package main

import (
	"cmp"
	"fmt"
	"os"

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

	"github.com/lhns/remote-docker/client/internal/config"
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

	// Held rather than raised: nothing may fail while the tree is being built.
	// See the PersistentPreRunE below.
	var session error
	if invokingDocker() {
		session = arrangeSession()
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
	//
	// The session error comes FIRST when both are present: a broken credential
	// helper is a warning about how images are pulled, and no session at all
	// is why the command cannot run.
	if deferred := cmp.Or(session, credentials); deferred != nil {
		cmd.PersistentPreRunE = func(*cobra.Command, []string) error { return deferred }
	}
	return cmd
}

// arrangeSession makes a session available for this invocation, and points the
// embedded CLI at it -- or does neither, when the invocation is aimed at a
// daemon that is not ours.
//
// Called only when the invocation is actually a docker command. The command
// tree is built for EVERY invocation, so doing this at build time would let
// `remote gc` -- or a bare `--help` -- probe the endpoint and open a whole
// file-serving session, which then races the real command's own inside one
// process.
//
// The error is REPORTED, not swallowed. Whatever stops a session existing is
// the reason the command about to run will fail, and without it the embedded
// CLI reaches an endpoint nobody is serving and reports a missing daemon --
// true, and useless, because the daemon is missing because of this.
func arrangeSession() error {
	// What the invocation is aimed at, which may be nothing of ours. See
	// target.go: a context we did not create is an instruction to talk to
	// somebody else, and honouring it means doing nothing at all here,
	// including saying nothing.
	aim := decideTarget(os.Args[1:], realLookups())
	if !aim.ensure {
		return nil
	}

	cfg, err := config.Resolve(config.Overrides{Workspace: aim.workspace}, "")
	if err != nil {
		return err
	}

	// Start one if nothing is serving, and replace one built from a different
	// commit when that costs nothing. Requiring `start` first would give the
	// embedded CLI, which exists so that nothing has to be installed, a setup
	// step of its own.
	endpoint := endpointOf(cfg)
	if err := ensureDaemon(cfg, endpoint); err != nil {
		return err
	}

	// Only where a context is not already pointing at it. DOCKER_HOST outranks
	// --context in docker's own resolution, so setting it here would override
	// the context we just agreed to honour.
	if aim.setHost {
		_ = os.Setenv("DOCKER_HOST", proxy.DockerHost(endpoint))
	}
	return nil
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
	// for the same reason: printing help is not worth a session either.
	switch scanRootArgs(os.Args[1:]).verb {
	case "", "remote", "context", "completion", "help":
		return false
	}
	return true
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

// newRootCommand is the whole command line: the Docker CLI, plus ours.
//
// The Docker CLI IS the root, rather than a subcommand of one. `docker run` is
// what a person types, and the program that has to stand in for docker should
// answer to that shape without an installation step in front of it. Renaming
// this binary to `docker` is then a complete installation, with no code behind
// it at all -- which is what replaced 550 lines of shim.
//
// Everything of ours is under `remote`, and nothing of ours is at this level.
// See remote.go for why the flags in particular had to move.
func newRootCommand() *cobra.Command {
	root := newDockerCommand()
	root.AddCommand(newRemoteCommand())
	return root
}
