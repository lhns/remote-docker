package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/lhns/remote-docker/internal/client/config"
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

	root.PersistentFlags().StringVar(&overrides.Host, "host", "", "workspace address")
	root.PersistentFlags().IntVar(&overrides.Port, "port", 0, "workspace SSH port")
	root.PersistentFlags().StringVarP(&overrides.User, "user", "u", "", "workspace account")
	root.PersistentFlags().StringVar(&overrides.Endpoint, "endpoint", "", "local Docker endpoint to serve")

	root.AddCommand(
		newEnrollCommand(),
		newStatusCommand(),
		newUpCommand(),
		newShellCommand(),
		newVersionCommand(),
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

			key, err := sshx.LoadOrCreateKey(config.KeyPath(), "remote-docker")
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintln(out, "Give this to whoever runs the workspace.")
			_, _ = fmt.Fprintf(out, "It must be saved as: authorized_keys.d/%s.pub\n", cfg.User)
			_, _ = fmt.Fprintln(out, "(the filename becomes your account name there)")
			_, _ = fmt.Fprintln(out)
			_, _ = fmt.Fprintln(out, key.AuthorizedKey(""))
			return nil
		},
	}
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
			if err := cfg.RequireHost(); err != nil {
				return err
			}

			ctx, cancel := signalContext()
			defer cancel()

			s, err := session.Open(ctx, session.Options{
				Config:   cfg,
				WorkDir:  mustWorkDir(),
				Endpoint: cfg.Endpoint,
				Log:      logger{},
			})
			if err != nil {
				return err
			}
			defer func() { _ = s.Close() }()

			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "%-20s %s@%s:%d\n", "workspace", cfg.User, cfg.Host, cfg.Port)
			_, _ = fmt.Fprintf(out, "%-20s %s (uid %d)\n", "account", s.Info.User, s.Info.UID)
			_, _ = fmt.Fprintf(out, "%-20s %d\n", "nfs port", s.Info.NFSPort)
			_, _ = fmt.Fprintf(out, "%-20s %s\n", "docker", s.Info.Docker)
			_, _ = fmt.Fprintf(out, "%-20s %s\n", "endpoint", s.Endpoint)
			return nil
		},
	}
}

func newUpCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "up",
		Short: "Open a session and serve the local Docker endpoint",
		Long: `Brings up the whole session and holds it open: this directory is exported
over the tunnel, the Docker endpoint is served locally, and published
container ports become reachable here as they start.

Point DOCKER_HOST at the printed endpoint and use docker normally.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
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
				Endpoint: cfg.Endpoint,
				Log:      logger{},
			})
			if err != nil {
				return err
			}
			defer func() { _ = s.Close() }()

			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintln(out)
			_, _ = fmt.Fprintf(out, "Docker endpoint ready. In another terminal:\n\n")
			_, _ = fmt.Fprintf(out, "    %s\n\n", exportLine(s.Endpoint))
			_, _ = fmt.Fprintln(out, "Then use docker normally. Ctrl-C here closes the session.")

			<-ctx.Done()
			_, _ = fmt.Fprintln(out, "\nclosing session")
			return nil
		},
	}
}

func newShellCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "shell",
		Short: "Open an interactive shell on the workspace",
		RunE: func(cmd *cobra.Command, _ []string) error {
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
				Endpoint: cfg.Endpoint,
				Log:      logger{},
			})
			if err != nil {
				return err
			}
			defer func() { _ = s.Close() }()

			return s.Shell(ctx)
		},
	}
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
