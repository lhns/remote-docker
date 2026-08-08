package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/command/commands"
	"github.com/docker/cli/cli/flags"
	"github.com/spf13/cobra"

	"github.com/lhns/remote-docker/internal/client/config"
	"github.com/lhns/remote-docker/internal/client/fswatch"
	"github.com/lhns/remote-docker/internal/client/proxy"
	"github.com/lhns/remote-docker/internal/client/session"
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

			// Nothing is serving that endpoint, so bring a session up for the
			// duration of this command rather than telling the user to go and
			// start one in another terminal. Requiring `up` first made the
			// embedded CLI -- the thing that exists so nothing has to be
			// installed -- the one part of this tool with a setup step.
			//
			// Which workspace this is comes from the same resolution as every
			// other command, so `--workspace ci docker ps` starts a session
			// for ci and answers from ci.
			if !proxy.Reachable(endpoint) {
				startImplicitSession(cfg, endpoint)
			}
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

// implicitSession is the one this command started for itself, if any. Closed
// by main once the command has finished, because a deferred close here would
// run while the docker command was still using it.
var implicitSession *session.Session

// startImplicitSession opens a session for the life of a single docker command.
//
// It serves files, because a bind mount is the whole point: `docker run -v
// .:/app` has to reach this machine's disk, and that needs the NFS export this
// session provides.
//
// Failure is deliberately quiet. The endpoint may have been taken by an `up`
// that started a moment ago -- the check above is a race by nature -- and in
// that case the docker command below connects to it and works. If there really
// is nothing there, docker reports it, which is a better message than anything
// guessed at here.
func startImplicitSession(cfg config.Config, endpoint string) {
	if cfg.Host == "" {
		return
	}
	watch, err := fswatch.ParseMode(cfg.Watch)
	if err != nil {
		watch = fswatch.ModeOff
	}
	s, err := session.Open(context.Background(), session.Options{
		Config:       cfg,
		WorkDir:      mustWorkDir(),
		Endpoint:     endpoint,
		Files:        session.FilesRequired,
		IdleTimeout:  cfg.IdleTimeout,
		Watch:        watch,
		WatchBudget:  cfg.WatchBudget,
		WatchExclude: cfg.WatchExclude,
		Log:          logger{},
	})
	if err != nil {
		return
	}
	implicitSession = s
}

// closeImplicitSession tears down a session this command started, warning if
// anything is left that will stop working without it.
//
// A container started with -d outlives the command that created it, but its
// bind mounts are NFS volumes served by THIS process. Closing takes the file
// server away and the container starts failing its I/O -- soft mounts make
// that an error rather than a hang, but it is still a container that was
// working and now is not. The user has to be told, and told what to do.
func closeImplicitSession() {
	if implicitSession == nil {
		return
	}
	if n := survivingContainers(); n > 0 {
		fmt.Fprintf(os.Stderr,
			"\nwarning: %d container(s) still hold directories served by this command,\n"+
				"and those mounts stop working now that it has finished.\n"+
				"Run `remote-docker up` in another terminal to keep them alive.\n", n)
	}
	_ = implicitSession.Close()
}

// survivingContainers counts containers that will outlive this command.
//
// Asked twice, because a foreground container is still running at the instant
// the command ends: `docker run` without -d has just sent it a signal and it
// takes a moment to go. Warning on that first answer told people their mounts
// were about to break every time they pressed Ctrl-C on a container that was
// already on its way out.
//
// The second look costs nothing in the ordinary case, because it only happens
// when the first one found something -- and finding something is exactly when
// being right matters.
func survivingContainers() int {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	n, err := implicitSession.OwnedVolumesInUse(ctx)
	if err != nil || n == 0 {
		return 0
	}
	select {
	case <-time.After(750 * time.Millisecond):
	case <-ctx.Done():
		return 0
	}
	n, err = implicitSession.OwnedVolumesInUse(ctx)
	if err != nil {
		return 0
	}
	return n
}
