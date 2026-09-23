package main

import (
	"fmt"
	"os"
	"os/exec"

	buildxcommands "github.com/docker/buildx/commands"

	// Drivers register themselves in init, normally from buildx's main.
	// Without them `docker build` answers "no drivers available".
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
// program's root (ADR 0009, ADR 0024).
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

		// Makes cobra add --version and -v, as the real CLI has at its root.
		Version: dockerVersionLine(),

		// Only the root's setting counts. Without it cobra parses every flag at
		// the deepest command, so `docker --context dev ps` handed --context to
		// `ps` and `compose -f x up` handed -f to `up`.
		TraverseChildren: true,

		// See unknown.go.
		Args: onlySubcommands,
		RunE: helpWhenBare,
	}
	// docker's own root setup. It puts the client options on Flags(), not
	// PersistentFlags(): --context's -c clashes with build's --cpu-shares and
	// panics `docker build --help` if persistent.
	opts, _ := cli.SetupRootCommand(cmd)

	// Held, not raised: see the PersistentPreRunE below.
	var session error
	if invokingDocker() {
		session = arrangeSession()
	}

	// After SetupRootCommand, whose template would print "Docker version" twice.
	cmd.SetVersionTemplate("{{.Version}}\n")

	dockerCli, err := command.NewDockerCli()
	if err != nil {
		// Reported at run time, so `--help` still works.
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

	// After Initialize, which loads the config file.
	credentials := checkCredentialHelpers(dockerCli.ConfigFile(), exec.LookPath, os.Stderr)

	commands.AddCommands(cmd, dockerCli)
	installModernBuilder(cmd, dockerCli)
	installCompose(cmd, dockerCli)

	// Raised when a command runs, never by leaving commands out of the tree:
	// that made `docker run --rm` fail with "unknown flag: --rm". Cobra runs
	// the closest PersistentPreRunE, so `docker stack` (which sets its own)
	// skips this. The session error wins: it is why the command cannot run.
	// Not cmp.Or: staticcheck misreads the generic's nil error as always true.
	deferred := session
	if deferred == nil {
		deferred = credentials
	}
	if deferred != nil {
		cmd.PersistentPreRunE = func(*cobra.Command, []string) error { return deferred }
	}
	return cmd
}

// arrangeSession makes a session available and points the embedded CLI at it,
// unless the invocation targets a daemon that is not ours (target.go). Only
// for docker commands: `remote gc` or `--help` must not open a session that
// races the command's own. Its error is returned, since otherwise the CLI
// reports only a missing daemon.
func arrangeSession() error {
	aim := decideTarget(os.Args[1:], realLookups())
	if !aim.ensure {
		return nil
	}

	cfg, err := config.Resolve(config.Overrides{Workspace: aim.workspace}, "")
	if err != nil {
		return err
	}

	endpoint := endpointOf(cfg)
	if err := ensureDaemon(cfg, endpoint); err != nil {
		return err
	}

	// Not when a context already points at it: DOCKER_HOST outranks --context.
	if aim.setHost {
		_ = os.Setenv("DOCKER_HOST", proxy.DockerHost(endpoint))
	}
	return nil
}

// NoSessionEnv is set on docker commands this program runs itself, since
// exec.LookPath("docker") may find us.
const NoSessionEnv = "REMOTE_DOCKER_NO_SESSION"

// invokingDocker reports whether this is a docker command that needs a
// session. Read from argv, since cobra has parsed nothing yet.
func invokingDocker() bool {
	if os.Getenv(NoSessionEnv) != "" {
		return false
	}

	// Commands that never reach a daemon, and no subcommand at all.
	switch scanRootArgs(os.Args[1:]).verb {
	case "", "remote", "context", "completion", "help":
		return false
	}
	return true
}

// installModernBuilder replaces `build` with buildx's, as docker does when
// the plugin is present. docker/cli alone silently builds with the classic
// builder, even with DOCKER_BUILDKIT=1 (ADR 0009).
func installModernBuilder(cmd *cobra.Command, dockerCli *command.DockerCli) {
	root := buildxcommands.NewRootCmd("buildx", true, dockerCli)

	for _, sub := range root.Commands() {
		if sub.Name() != "build" {
			continue
		}
		// Detached first: a command still under another root inherits its
		// flags and help.
		root.RemoveCommand(sub)
		if old, _, err := cmd.Find([]string{"build"}); err == nil {
			cmd.RemoveCommand(old)
		}
		sub.Use = "build [OPTIONS] PATH | URL | -"
		cmd.AddCommand(sub)
		break
	}

	// The buildx root is not registered: without the plugin harness,
	// `docker buildx version` panics on a nil dereference.
}

// newRootCommand is the Docker CLI with ours under `remote` (ADR 0024).
func newRootCommand() *cobra.Command {
	root := newDockerCommand()
	root.AddCommand(newRemoteCommand())
	return root
}
