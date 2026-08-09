package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/lhns/remote-docker/internal/client/config"
	"github.com/lhns/remote-docker/internal/client/proxy"
	"github.com/lhns/remote-docker/internal/client/session"
	"github.com/lhns/remote-docker/internal/client/sshx"
)

// version is set at build time by the release workflow.
var version = "dev"

// overrides collects the global flags.
var overrides config.Overrides

func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "remote-docker",
		Short: "Run Docker on a remote workspace with your local files really mounted",
		Long: `remote-docker gives you a Docker daemon on a remote workspace while your
own directories are genuinely mounted into the containers -- not copied, not
synced -- so bind mounts, published ports and the standard Docker tooling all
behave the way they would locally.

Nothing needs to be installed on this machine beyond this binary.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	// No shorthands, and that is not an oversight.
	//
	// Cobra merges a root's persistent flags into every subcommand, and this
	// root has the entire Docker CLI underneath it. pflag SKIPS a flag whose
	// long name is already taken -- which is why --user and --host coexist
	// with docker's own -- but a clashing SHORTHAND panics outright:
	//
	//	panic: unable to redefine 'w' shorthand in "run" flagset:
	//	       it's already used for "workdir" flag
	//
	// So `remote-docker docker run -ti debian bash` crashed, because -w meant
	// --workspace here and --workdir there. A shorthand that turns a whole
	// subcommand tree into a panic is worth less than typing the long form.
	root.PersistentFlags().StringVar(&overrides.Workspace, "workspace", "", "which configured workspace to use")
	root.PersistentFlags().StringVar(&overrides.Host, "host", "", "workspace address")
	root.PersistentFlags().IntVar(&overrides.Port, "port", 0, "workspace SSH port")
	root.PersistentFlags().StringVar(&overrides.User, "user", "", "workspace account")
	root.PersistentFlags().StringVar(&overrides.Endpoint, "endpoint", "", "local Docker endpoint to serve")

	root.AddCommand(
		newEnrollCommand(),
		newStatusCommand(),
		newUpCommand(),
		newGCCommand(),
		newDockerCommand(),
		newVersionCommand(),
		newWorkspaceCommand(),
		newStartCommand(),
		newStopCommand(),
		newRestartCommand(),
	)
	return root
}

func resolve() (config.Config, error) {
	return config.Resolve(overrides, "")
}

// logger prints session progress to stderr, so stdout stays usable for
// anything a command genuinely outputs.
type logger struct{}

func (logger) Printf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "  "+format+"\n", args...)
}

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
// `Files: NoFiles` is the load-bearing part, and the reason this is shared
// rather than written twice. An account has exactly one reverse-tunnel port
// (ADR 0003), so a command that does not need to export files must not try --
// it would fail the moment a session is running, which is precisely when
// somebody runs `status` or `gc`.
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
		Files:    session.NoFiles,
		Log:      logger{},
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
		Short: "Show what the workspace reports about this account",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()

			return withQuerySession(func(ctx context.Context, s *session.Session) error {
				// status is the one command whose whole job is to report what
				// the workspace says, so it connects rather than waiting to be
				// asked.
				info, err := s.Info(ctx)
				if err != nil {
					return err
				}

				row(out, "name", cfg.Name)
				rowf(out, "workspace", "%s@%s:%d", cfg.User, cfg.Host, cfg.Port)
				rowf(out, "account", "%s (uid %d)", info.User, info.UID)
				rowf(out, "nfs port", "%d", info.NFSPort)
				row(out, "docker", info.Docker)
				row(out, "mode", info.Mode)

				// Said plainly, because vfs is the difference between a
				// container starting in a second and in minutes, and nothing
				// about it fails. Reaching the daemon's own host to look is
				// exactly what an account may not do, so this is the only
				// place it can be seen.
				switch info.Storage {
				case "":
					// An agent too old to report it, or a daemon not started.
				case "vfs":
					row(out, "storage", "vfs -- SLOW: every container create copies the whole image")
				default:
					row(out, "storage", info.Storage)
				}

				// The agent's build. A different question from the local
				// version, and the one that matters when the workspace behaves
				// oddly.
				//
				// Reported even when empty rather than skipped: a workspace too
				// old to send it looks exactly like one that failed to, and
				// silence would leave an answerable question unanswerable.
				agent := info.Agent
				if agent == "" {
					agent = "not reported (workspace predates it)"
				}
				row(out, "agent", agent)

				row(out, "endpoint", s.Endpoint)
				reportLocalSession(out, cfg)
				return nil
			})
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
		Long: `Each distinct directory bound into a container gets a volume on the
workspace, and they outlive the containers that referenced them.

Only volumes this client created, for this account, and referenced by no
container -- running or stopped -- are removed. A volume you created yourself
is never touched, whatever it is named.`,
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
// `status`, `workspace inspect` and reportLocalSession print one table between
// them -- status calls reportLocalSession -- so the width has to agree across
// all three. It was a bare %-20s at thirteen call sites.
func row(out io.Writer, key, value string) {
	if value != "" {
		_, _ = fmt.Fprintf(out, "%-20s %s\n", key, value)
	}
}

// rowf is row with a formatted value.
func rowf(out io.Writer, key, format string, args ...any) {
	row(out, key, fmt.Sprintf(format, args...))
}
