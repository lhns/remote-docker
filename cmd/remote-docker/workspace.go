package main

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"github.com/lhns/remote-docker/internal/client/config"
	"github.com/lhns/remote-docker/internal/client/proxy"
	"github.com/lhns/remote-docker/internal/client/sshx"
)

// The workspace command manages ~/.remote-docker.json, which until now could
// only be listed -- adding one meant writing JSON by hand, and the "no
// workspaces configured" message said so, which is not a thing a CLI should
// ever have to admit.
//
// Docker contexts are written as a side effect rather than by a separate
// command. There is no case where you want a workspace configured and not
// reachable as `docker --context <name>`, so making that a second thing to
// remember was a split in the tool that was never a split in the task.
//
// The verbs are docker's -- create, ls, use, rm, inspect -- because a
// workspace IS the thing a docker context points at, and borrowing the
// vocabulary costs nothing and saves explaining. The noun stays ours: the
// config file's key is `workspaces`, the wire protocol is `workspace-info`,
// the server's variables are WORKSPACE_*, and a CLI that disagreed with all
// of them would trade one confusion for another. The old verbs remain as
// aliases.
func newWorkspaceCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "workspace",
		Aliases: []string{"workspaces"},
		Short:   "Add, remove and list your workspaces",
	}
	cmd.AddCommand(
		newWorkspaceAddCommand(),
		newWorkspaceRemoveCommand(),
		newWorkspaceListCommand(),
		newWorkspaceDefaultCommand(),
		newWorkspaceInspectCommand(),
	)
	// Bare `remote-docker workspaces` still lists, which is what it did
	// before this command existed.
	cmd.RunE = newWorkspaceListCommand().RunE
	return cmd
}

func newWorkspaceAddCommand() *cobra.Command {
	var host, user, endpoint, watch string
	var port int
	var makeDefault, noContext bool

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
			if port == 0 {
				port = config.DefaultSSHPort
			}

			file, err := config.Load("")
			if err != nil {
				return err
			}
			_, existed := file.Workspaces[name]

			ws := config.Workspace{Host: host, Port: port, User: user, Endpoint: endpoint, Watch: watch}
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
			_, _ = fmt.Fprintf(out, "%s workspace %q: %s@%s:%d\n", verb, name, cfg.User, cfg.Host, cfg.Port)

			if !noContext {
				reportContext(out, cfg)
			}

			// The key is per machine, not per workspace, so it is the same one
			// every workspace has to accept -- and a freshly added workspace
			// is exactly when someone has not yet handed it over.
			_, _ = fmt.Fprintf(out,
				"\nIf this machine is not enrolled there yet, hand this to whoever runs it:\n\n    %s\n",
				enrolledKey())
			return nil
		},
	}

	cmd.Flags().StringVar(&host, "host", "", "workspace address (required)")
	cmd.Flags().IntVar(&port, "port", 0, "ssh port")
	cmd.Flags().StringVar(&user, "user", "", "workspace account; defaults to your local username")
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "override where the local Docker API is served")
	cmd.Flags().StringVar(&watch, "watch", "", "replay file changes: off, partial or coarse")
	cmd.Flags().BoolVar(&makeDefault, "default", false, "make this the default workspace")
	cmd.Flags().BoolVar(&noContext, "no-context", false, "do not create a docker context")
	return cmd
}

func newWorkspaceRemoveCommand() *cobra.Command {
	var keepContext bool

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

			// Resolved BEFORE removal, because the context name is derived
			// from the workspace and there is nothing to derive it from
			// afterwards.
			cfg, cfgErr := config.Resolve(config.Overrides{Workspace: name}, "")

			if !file.Remove(name) {
				return fmt.Errorf("no workspace named %q; `remote-docker workspace ls` shows what there is", name)
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
					"no default workspace now; set one with `remote-docker workspace use <name>`\n")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&keepContext, "keep-context", false, "leave the docker context in place")
	return cmd
}

func newWorkspaceDefaultCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "use <name>",
		Aliases: []string{"default"},
		Short:   "Choose which workspace commands use when none is named",
		Args:    cobra.ExactArgs(1),
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
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "default workspace is now %q\n", name)
			return nil
		},
	}
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
					_, _ = fmt.Fprintf(out, "one unnamed workspace: %s@%s:%d\n",
						file.User, file.Host, file.Port)
					return nil
				}
				// Previously this printed the JSON to write by hand. There is
				// a command for it now.
				_, _ = fmt.Fprintf(out,
					"no workspaces configured. Add one:\n\n"+
						"    remote-docker workspace create dev --host dev.example --user alice\n")
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
				_, _ = fmt.Fprintf(out, "%s%-13s %-30s %s\n",
					marker, name,
					fmt.Sprintf("%s@%s:%d", cfg.User, cfg.Host, cfg.Port),
					proxy.DockerHost(cfg.EndpointFor(proxy.DefaultEndpoint)))
			}
			_, _ = fmt.Fprintln(out, "\n* default")
			return nil
		},
	}
}

// reportContext creates the docker context for a workspace, reporting rather
// than failing.
//
// Not having a docker CLI is an ordinary situation here -- the binary carries
// its own, and the whole premise is a machine where nothing is installed -- so
// it must not turn adding a workspace into an error.
func reportContext(out interface{ Write([]byte) (int, error) }, cfg config.Config) {
	docker, err := exec.LookPath("docker")
	if err != nil {
		_, _ = fmt.Fprintf(out, "no docker CLI on PATH, so no context was created; "+
			"use the built-in one with `remote-docker docker ...`\n")
		return
	}
	installed, err := installContext(docker, cfg)
	if err != nil {
		_, _ = fmt.Fprintf(out, "workspace saved, but its docker context was not created: %v\n", err)
		return
	}
	_, _ = fmt.Fprintf(out, "docker context %q -> %s\n", installed.name, installed.endpoint)
}

func removeContextFor(out interface{ Write([]byte) (int, error) }, cfg config.Config) {
	docker, err := exec.LookPath("docker")
	if err != nil {
		return
	}
	name := cfg.ContextName()
	if !contextIsOurs(docker, name) {
		return
	}
	// Select default first: removing the active context would leave the CLI
	// pointing at one that no longer exists.
	_ = exec.Command(docker, "context", "use", "default").Run()
	if err := exec.Command(docker, "context", "rm", "-f", name).Run(); err != nil {
		_, _ = fmt.Fprintf(out, "docker context %q was left in place: %v\n", name, err)
		return
	}
	_, _ = fmt.Fprintf(out, "removed docker context %q\n", name)
}

// enrolledKey returns this machine's public key line, or a hint if it cannot
// be read. Never fatal: it is printed as advice.
func enrolledKey() string {
	kp, err := sshx.LoadOrCreateKey(config.KeyPath(), config.KeyComment())
	if err != nil {
		return "(run `remote-docker enroll` to generate one)"
	}
	return strings.TrimSpace(kp.AuthorizedKey(config.KeyComment()))
}

// newWorkspaceInspectCommand shows everything about one workspace in one place.
//
// It exists because the pieces were scattered: the config file holds the host
// and account, the endpoint is derived from the name, the docker context is
// named after it too, and a session may or may not be running against it.
// Answering "what is this workspace, actually" meant knowing all four
// derivations. Now it does not.
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
				return fmt.Errorf("no workspace is configured; add one with " +
					"`remote-docker workspace create <name> --host <host>`")
			}

			out := cmd.OutOrStdout()
			row := func(k, v string) {
				if v != "" {
					_, _ = fmt.Fprintf(out, "%-20s %s\n", k, v)
				}
			}
			row("name", cfg.Name)
			row("workspace", fmt.Sprintf("%s@%s:%d", cfg.User, cfg.Host, cfg.Port))
			row("endpoint", proxy.DockerHost(cfg.EndpointFor(proxy.DefaultEndpoint)))
			row("docker context", cfg.ContextName())
			row("watch", cfg.Watch)
			for _, ex := range cfg.WatchExclude {
				row("watch exclude", ex)
			}
			reportLocalSession(out, cfg)
			return nil
		},
	}
}
