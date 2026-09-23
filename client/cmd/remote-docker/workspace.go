package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/lhns/remote-docker/client/internal/config"
	"github.com/lhns/remote-docker/core-client/keys"
	"github.com/lhns/remote-docker/core/workspace"
	"github.com/lhns/remote-docker/machine"
)

// The workspaces in ~/.remote-docker.json, each with a docker context written
// as a side effect. Reached as `remote create` etc. (ADR 0024); the code keeps
// the noun "workspace", as the config, wire protocol and agent do.
func newWorkspaceCreateCommand() *cobra.Command {
	var host, user, endpoint, watch, consistency, caFile string
	var port int
	var makeDefault, noContext, insecure bool

	cmd := &cobra.Command{
		Use:     "create <name>",
		Aliases: []string{"add"},
		Short:   "Create a workspace and its docker context",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if host == "" {
				return fmt.Errorf("--host is required: there is nothing to connect to without it")
			}
			// Only for a bare host: Transport refuses a port beside a URL.
			if port == 0 && !strings.Contains(host, "://") {
				port = config.DefaultSSHPort
			}

			file, err := config.Load("")
			if err != nil {
				return err
			}
			_, existed := file.Workspaces[name]

			// Refused here rather than on the first container.
			if _, err := workspace.ParseMode(consistency); err != nil {
				return err
			}

			ws := config.Workspace{
				Host: host, Port: port, User: user, Endpoint: endpoint, Watch: watch,
				Consistency: consistency, CAFile: caFile, Insecure: insecure,
			}
			if err := file.Set(name, ws); err != nil {
				return err
			}
			if makeDefault {
				file.Default = name
			}
			if err := config.Save(file, ""); err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			verb := "added"
			if existed {
				verb = "updated"
			}
			cfg, err := config.Resolve(config.Overrides{Workspace: name}, "")
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(out, "%s workspace %q: %s\n", verb, name, where(cfg))

			if !noContext {
				reportContext(out, cfg)
			}

			_, _ = fmt.Fprintf(out,
				"\nIf this machine is not enrolled there yet, hand this to whoever runs it:\n\n    %s\n",
				enrolledKey())
			return nil
		},
	}

	cmd.Flags().StringVar(&host, "host", "",
		"workspace address (required): a host, or ssh://, ws:// or wss:// with one")
	cmd.Flags().StringVar(&caFile, "ca-file", "",
		"verify a ws:// endpoint against this CA instead of the system roots")
	cmd.Flags().BoolVar(&insecure, "insecure", false,
		"accept any certificate from a ws:// endpoint; ssh still authenticates both ends")
	cmd.Flags().IntVar(&port, "port", 0, "ssh port")
	cmd.Flags().StringVar(&user, "user", "", "workspace account; defaults to your local username")
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "override where the local Docker API is served")
	cmd.Flags().StringVar(&watch, "watch", "", "replay file changes: off, partial or coarse")
	cmd.Flags().StringVar(&consistency, "consistency", "",
		"how shares are mounted: read=<direct|cached>,write=<through|back|ephemeral>")
	cmd.Flags().BoolVar(&makeDefault, "default", false, "make this the default workspace")
	cmd.Flags().BoolVar(&noContext, "no-context", false, "do not create a docker context")
	return cmd
}

func newWorkspaceRemoveCommand() *cobra.Command {
	var keepContext, keepMachine bool

	cmd := &cobra.Command{
		Use:     "rm <name>",
		Aliases: []string{"remove"},
		Short:   "Remove a workspace and its docker context",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			file, err := config.Load("")
			if err != nil {
				return err
			}

			// Before removal: the context name derives from the entry.
			cfg, cfgErr := config.Resolve(config.Overrides{Workspace: name}, "")

			// Destroyed before the entry goes: the entry is the only record the
			// machine exists, so a failure must leave it to retry from.
			machine := file.Workspaces[name].Machine
			if machine != nil && !keepMachine {
				if err := destroyMachine(cmd, machine); err != nil {
					return err
				}
			}

			if !file.Remove(name) {
				return fmt.Errorf("no workspace named %q; `%s` shows what there is", name, ourCommand("ls"))
			}
			if err := config.Save(file, ""); err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "removed workspace %q\n", name)

			if !keepContext && cfgErr == nil {
				removeContextFor(out, cfg)
			}
			if file.Default == "" && len(file.Names()) > 1 {
				_, _ = fmt.Fprintf(out,
					"no default workspace now; set one with `%s`\n", ourCommand("use <name>"))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&keepContext, "keep-context", false, "leave the docker context in place")
	cmd.Flags().BoolVar(&keepMachine, "keep-machine", false,
		"leave the local machine running instead of destroying it")
	return cmd
}

// destroyMachine destroys a workspace's machine. An unknown backend is an
// error, so `rm` refuses rather than orphaning a running machine.
func destroyMachine(cmd *cobra.Command, m *config.Machine) error {
	backend, err := machine.Find(m.Backend)
	if err != nil {
		return fmt.Errorf("cannot destroy the %s machine %q: %w", m.Backend, m.Name, err)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "destroying the %s machine %q\n", m.Backend, m.Name)
	if err := backend.Destroy(cmd.Context(), m.Name); err != nil {
		return fmt.Errorf("destroying the %s machine %q: %w", m.Backend, m.Name, err)
	}
	return nil
}

func newWorkspaceUseCommand() *cobra.Command {
	var noContext bool

	cmd := &cobra.Command{
		Use:     "use <name>",
		Aliases: []string{"default"},
		Short:   "Make a workspace the default and select its docker context",
		Long: `Makes this the workspace the "remote" commands use, and selects its docker
context, so compose and other docker tools use it too.

Creates the context first if it is missing.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			file, err := config.Load("")
			if err != nil {
				return err
			}
			if _, ok := file.Workspaces[name]; !ok {
				return fmt.Errorf("no workspace named %q", name)
			}
			file.Default = name
			if err := config.Save(file, ""); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "default workspace is now %q\n", name)

			if noContext {
				return nil
			}

			// Docker's current context, which every other tool reads. Ensured
			// first, since a workspace made with --no-context has none.
			cfg, err := config.Resolve(config.Overrides{Workspace: name}, "")
			if err != nil {
				return nil
			}
			reportContext(out, cfg)
			useContext(out, cfg.ContextName())
			return nil
		},
	}

	cmd.Flags().BoolVar(&noContext, "no-context", false, "do not select the docker context")
	return cmd
}

func newWorkspaceListCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List the configured workspaces",
		RunE: func(cmd *cobra.Command, _ []string) error {
			file, err := config.Load("")
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			names := file.Names()
			if len(names) == 0 {
				if file.Host != "" {
					_, _ = fmt.Fprintf(out, "one unnamed workspace: %s\n",
						where(config.Config{User: file.User, Host: file.Host, Port: file.Port}))
					return nil
				}
				_, _ = fmt.Fprintf(out,
					"no workspaces configured. Add one:\n\n"+
						"    %s\n", ourCommand("create dev --host dev.example --user alice"))
				return nil
			}

			_, _ = fmt.Fprintf(out, "%-14s %-30s %s\n", "NAME", "WORKSPACE", "ENDPOINT")
			for _, name := range names {
				cfg, err := config.Resolve(config.Overrides{Workspace: name}, "")
				if err != nil {
					_, _ = fmt.Fprintf(out, "%-14s %v\n", name, err)
					continue
				}
				marker := " "
				if name == file.Default {
					marker = "*"
				}
				where := where(cfg)
				if m := file.Workspaces[name].Machine; m != nil {
					// Shown because `rm` destroys it.
					where += " (" + m.Backend + ")"
				}
				_, _ = fmt.Fprintf(out, "%s%-13s %-30s %s\n",
					marker, name, where, dockerHostOf(cfg))
			}
			_, _ = fmt.Fprintln(out, "\n* default")
			return nil
		},
	}
}

// where names a workspace the way it is reached, so a wss:// host does not
// print as though it had an SSH port on the end of it.
func where(cfg config.Config) string {
	transport, err := cfg.Transport()
	if err != nil {
		return fmt.Sprintf("%s@%s", cfg.User, cfg.Host)
	}
	return fmt.Sprintf("%s@%s", cfg.User, transport)
}

// reportContext creates the docker context for a workspace, reporting rather
// than failing. A workspace is still usable without one.
func reportContext(out io.Writer, cfg config.Config) {
	installed, err := installContext(cfg)
	if err != nil {
		_, _ = fmt.Fprintf(out, "workspace saved, but its docker context was not created: %v\n", err)
		return
	}
	_, _ = fmt.Fprintf(out, "docker context %q -> %s\n", installed.name, installed.endpoint)
}

// useContext selects a workspace's context as docker's current one, reporting
// rather than failing.
func useContext(out io.Writer, name string) {
	if outBytes, err := dockerCmd("context", "use", name).CombinedOutput(); err != nil {
		_, _ = fmt.Fprintf(out, "docker context %q was not selected: %v: %s\n",
			name, err, strings.TrimSpace(string(outBytes)))
		return
	}
	_, _ = fmt.Fprintf(out, "docker context is now %q\n", name)
}

func removeContextFor(out io.Writer, cfg config.Config) {
	name := cfg.ContextName()
	if !contextIsOurs(name) {
		// Said out loud: it may also be ours with a marker that failed to write.
		_, _ = fmt.Fprintf(out, "docker context %q was left in place: "+
			"it is not marked as one remote-docker created\n", name)
		return
	}
	// Deselect first, or the CLI is left pointing at a missing context.
	_ = dockerCmd("context", "use", "default").Run()

	if out2, err := dockerCmd("context", "rm", "-f", name).CombinedOutput(); err != nil {
		_, _ = fmt.Fprintf(out, "docker context %q was left in place: %v: %s\n",
			name, err, strings.TrimSpace(string(out2)))
		return
	}
	_, _ = fmt.Fprintf(out, "removed docker context %q\n", name)
}

// enrolledKey returns this machine's public key line, or a hint if it cannot
// be read. Never fatal: it is printed as advice.
func enrolledKey() string {
	kp, err := keys.LoadOrCreateKey(config.KeyPath(), config.KeyComment())
	if err != nil {
		return "(run `" + ourCommand("enroll") + "` to generate one)"
	}
	return strings.TrimSpace(kp.AuthorizedKey(config.KeyComment()))
}

// newWorkspaceInspectCommand shows everything about one workspace in one place.
func newWorkspaceInspectCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "inspect [name]",
		Short: "Show a workspace's settings, endpoint and docker context",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			cfg, err := config.Resolve(config.Overrides{Workspace: name}, "")
			if err != nil {
				return err
			}
			if cfg.Host == "" {
				return fmt.Errorf("no workspace is configured; add one with `%s`",
					ourCommand("create <name> --host <host>"))
			}

			out := cmd.OutOrStdout()
			row(out, "name", cfg.Name)

			transport, err := cfg.Transport()
			if err != nil {
				rowf(out, "workspace", "%s@%s (%v)", cfg.User, cfg.Host, err)
			} else {
				rowf(out, "workspace", "%s@%s", cfg.User, transport)
			}
			if cfg.Insecure {
				row(out, "tls", "NOT verified (--insecure); ssh still authenticates both ends")
			} else if cfg.CAFile != "" {
				row(out, "tls", "verified against "+cfg.CAFile)
			}
			row(out, "endpoint", dockerHostOf(cfg))
			row(out, "docker context", cfg.ContextName())
			row(out, "watch", cfg.Watch)
			row(out, "consistency", cfg.Consistency)
			for _, ex := range cfg.WatchExclude {
				row(out, "watch exclude", ex)
			}
			reportLocalSession(out, cfg)
			return nil
		},
	}
}
