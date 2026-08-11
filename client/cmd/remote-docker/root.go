package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/lhns/remote-docker/client/internal/config"
	"github.com/lhns/remote-docker/client/internal/proxy"
	"github.com/lhns/remote-docker/client/internal/session"
	"github.com/lhns/remote-docker/client/internal/sshx"
	"github.com/lhns/remote-docker/internal/logx"
)

// version is set at build time by the release workflow.
var version = "dev"

// overrides collects the global flags.
var overrides config.Overrides

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

func resolve() (config.Config, error) {
	return config.Resolve(overrides, "")
}

// logger prints session progress to stderr, so stdout stays usable for
// anything a command genuinely outputs.
//
// Two spaces and the message, which is what these lines have always looked
// like: they sit under a command's own output and are read by a person, not
// parsed. logx.Handler is what keeps that true through log/slog, whose own
// TextHandler would render them as time=... level=INFO msg="...".
func logger() *slog.Logger { return logx.Logger(os.Stderr, "  ", false) }

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), version)
			return nil
		},
	}
}

func newEnrollCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "enroll",
		Short: "Print the public key to hand to whoever runs the workspace",
		Long: `Generates this machine's keypair on first use and prints the public half.

Enrolment is out of band: someone with access to the workspace saves the key
as authorized_keys.d/<your account>.pub, and the filename becomes your unix
account there.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}

			// config.KeyComment(), not a bare "remote-docker": the comment is the
			// only thing distinguishing one .pub from another in the
			// workspace's authorized_keys.d, and the person adding it needs
			// to know whose machine it came from. LoadOrCreateKey only sets a
			// comment when it GENERATES, and enroll is what usually generates,
			// so this is the spelling that ends up on almost every key.
			key, err := sshx.LoadOrCreateKey(config.KeyPath(), config.KeyComment())
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintln(out, "Give this to whoever runs the workspace.")
			_, _ = fmt.Fprintf(out, "It must be saved as: authorized_keys.d/%s.pub\n", cfg.User)
			_, _ = fmt.Fprintln(out, "(the filename becomes your account name there)")
			_, _ = fmt.Fprintln(out)
			// With the comment, not without. It is the only thing telling
			// whoever files this .pub which machine it came from, and an
			// authorized_keys.d full of anonymous keys cannot be audited.
			_, _ = fmt.Fprintln(out, key.AuthorizedKey(config.KeyComment()))
			return nil
		},
	}
}

// withQuerySession opens a session that only ASKS the workspace things, runs
// fn against it, and closes it.
//
// `session.Query` is the load-bearing part, and the reason this is shared
// rather than written twice. A query session takes neither the local endpoint
// nor the account's one reverse-tunnel port (ADR 0003), so it still works while
// a real session holds both, which is precisely when somebody runs `status`
// or `gc`. See session.Role for what each half of that prevents.
func withQuerySession(fn func(ctx context.Context, s *session.Session) error) error {
	cfg, err := resolve()
	if err != nil {
		return err
	}
	if err := cfg.RequireHost(); err != nil {
		return err
	}

	ctx, cancel := signalContext()
	defer cancel()

	s, err := session.Open(ctx, session.Options{
		Config:   cfg,
		WorkDir:  mustWorkDir(),
		Endpoint: endpointOf(cfg),
		Role:     session.Query,
		Log:      logger(),
	})
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()

	return fn(ctx, s)
}

func newStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Is this working, and what is it talking to?",
		Long: `Prints a verdict first: ready, or the first thing that is wrong.

Then the detail behind it, grouped by question: whether a session is up and
how other tools reach it, what is on the other end, and which builds are in
play.

Reports what it can even when the workspace cannot be reached, which is when
somebody is most likely to be running it.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			// The one thing worth failing on: with no host there is no
			// workspace to have a status.
			if err := cfg.RequireHost(); err != nil {
				return err
			}

			f := gather(cfg)
			f.askWorkspace()
			reportStatus(cmd.OutOrStdout(), f)
			return nil
		},
	}
}

// newUpCommand is what `start --foreground` used to be called.
//
// Hidden rather than deleted. It is in shell history, in scripts and possibly
// in a systemd unit, and a command that still works costs one line while a
// command that stopped existing costs somebody an afternoon. It is the same
// code path, so there is no second behaviour to keep in step.
func newUpCommand() *cobra.Command {
	cmd := newStartCommand()
	cmd.Use = "up"
	cmd.Short = "Deprecated: use `start --foreground`"
	cmd.Long = `Runs a session in this terminal and holds it open.

Superseded by "remote-docker start --foreground", which is the same thing.
Kept working because it appears in scripts.`
	cmd.Hidden = true

	// Foreground by default, because that is what `up` always did. `start` on
	// its own detaches now, and silently changing that underneath an existing
	// script would be worse than not keeping the alias at all.
	if f := cmd.Flags().Lookup("foreground"); f != nil {
		_ = f.Value.Set("true")
		f.DefValue = "true"
	}
	return cmd
}

// exportLine renders the DOCKER_HOST assignment for the shell the user is
// most likely holding.
func exportLine(endpoint string) string {
	if os.PathSeparator == '\\' {
		return fmt.Sprintf("$env:DOCKER_HOST = %q", endpoint)
	}
	return fmt.Sprintf("export DOCKER_HOST=%s", endpoint)
}

func mustWorkDir() string {
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	return dir
}

// signalContext cancels on Ctrl-C so a session is torn down rather than
// leaving its reverse forward bound on the workspace.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func newGCCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "gc",
		Short: "Remove share volumes this account is no longer using",
		Long: `Each directory bound into a container gets a volume on the workspace, and
they outlive the containers that used them.

Only volumes this client created, for this account, that no container refers
to. A volume you created yourself is never touched, whatever it is named, and
neither is the one for the directory this runs in.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withQuerySession(func(ctx context.Context, s *session.Session) error {
				removed, err := s.Collect(ctx)
				if err != nil {
					return err
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "removed %d unused share volume(s)\n", removed)
				return nil
			})
		},
	}
}

// endpointOf is where this workspace's Docker API is served locally.
//
// One spelling, in one place, and that is the point rather than the brevity.
// `endpointOf(cfg)` was written out at ten call
// sites, and the argument is the part that matters: passing an empty base
// instead used to derive the RELATIVE path "-dev" for a named workspace, which
// meant a socket in whatever directory the process happened to be in and a
// docker context reading unix://-dev. That could not be reproduced on Windows,
// where the pipe name is a real constant, and CI never saw it because the
// suites set an endpoint explicitly.
func endpointOf(cfg config.Config) string {
	return cfg.EndpointFor(proxy.DefaultEndpoint())
}

// dockerHostOf is the same endpoint as a DOCKER_HOST value.
func dockerHostOf(cfg config.Config) string {
	return proxy.DockerHost(endpointOf(cfg))
}

// row prints one aligned "key    value" line.
//
// `status` and `workspace inspect` print one table each and share this width,
// so a row added to one lines up in the other. It was a bare %-20s at thirteen
// call sites.
func row(out io.Writer, key, value string) {
	if value != "" {
		_, _ = fmt.Fprintf(out, "%-20s %s\n", key, value)
	}
}

// rowf is row with a formatted value.
func rowf(out io.Writer, key, format string, args ...any) {
	row(out, key, fmt.Sprintf(format, args...))
}
