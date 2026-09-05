package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/lhns/remote-docker/client/internal/config"
	"github.com/lhns/remote-docker/client/internal/machine"
	"github.com/lhns/remote-docker/core-client/keys"
	"github.com/lhns/remote-docker/core/workspace"
)

// The workspaces in ~/.remote-docker.json.
//
// Docker contexts are written as a side effect rather than by a separate
// command. There is no case where you want a workspace configured and not
// reachable as `docker --context <name>`, so making that a second thing to
// remember was a split in the tool that was never a split in the task.
//
// The verbs are docker's (create, ls, use, rm, inspect) because a workspace IS
// the thing a docker context points at, and borrowing the vocabulary costs
// nothing and saves explaining. They are reached as `remote create` and so on,
// because a remote IS a workspace (ADR 0024), but the noun stays ours in the
// code: the config file's key is `workspaces`, the wire protocol is
// `workspace-info`, the server's variables are WORKSPACE_*, and a CLI that
// disagreed with all of them would trade one confusion for another. The old
// verbs remain as aliases.
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
			// Only for a bare host. A host carrying a scheme says its own port,
			// or takes the default its scheme implies, and writing 2222 beside
			// a wss:// URL would be the contradiction Transport refuses.
			if port == 0 && !strings.Contains(host, "://") {
				port = config.DefaultSSHPort
			}

			file, err := config.Load("")
			if err != nil {
				return err
			}
			_, existed := file.Workspaces[name]

			// Parsed before it is written, so a word nothing understands is
			// refused here rather than on the first container.
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

			// The key is per machine, not per workspace, so it is the same one
			// every workspace has to accept, and a freshly added workspace
			// is exactly when someone has not yet handed it over.
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

			// Resolved BEFORE removal, because the context name is derived
			// from the workspace and there is nothing to derive it from
			// afterwards.
			cfg, cfgErr := config.Resolve(config.Overrides{Workspace: name}, "")

			// And the machine, for the same reason and a heavier one: the
			// config entry is the only record that a Linux system was ever
			// built for this workspace. Delete it first and the machine is
			// still running on somebody's laptop with nothing naming it.
			//
			// Destroyed BEFORE the entry goes, so a failure leaves a workspace
			// that still knows about its machine and can be told to try again.
			// The other order leaves an orphan and no way to ask for it back.
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

// destroyMachine takes away the Linux system this program built for a
// workspace.
//
// A backend that is not compiled into this build is reported rather than
// ignored. Removing the config entry anyway would leave a machine running with
// nothing on the system naming it, which is worse than refusing: the user can
// at least be told what to remove by hand.
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

			// And docker's, which is what everything that is not this binary
			// resolves. The context is ensured first: a workspace created on a
			// machine with no docker CLI, or with --no-context, has none yet,
			// and selecting one that does not exist only produces an error.
			cfg, err := config.Resolve(config.Overrides{Workspace: name}, "")
			if err != nil {
				return nil
			}
			reportContext(out, cfg)
			useContext(out, cfg.ContextName())
			return nil
		},
	}

	// Same spelling and same meaning as `create --no-context`: leave docker's
	// context alone. Someone who created a workspace without one has said what
	// they want, and `use` must not quietly overrule it.
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
					// Which backend, because `rm` will destroy it and the
					// person reading this table is usually deciding whether
					// that is what they want.
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
		// Whatever is wrong with it, the raw setting is what the user typed and
		// what they will recognise; Transport's own error says the rest.
		return fmt.Sprintf("%s@%s", cfg.User, cfg.Host)
	}
	return fmt.Sprintf("%s@%s", cfg.User, transport)
}

// reportContext creates the docker context for a workspace, reporting rather
// than failing. A workspace is still usable without one.
//
// Having no docker CLI on PATH is ordinary here and no longer stops it: this
// binary is one, and dockerCmd falls back to it. Giving up left the premise
// machine with no context, so every tool that resolves one found the platform
// default instead.
func reportContext(out io.Writer, cfg config.Config) {
	installed, err := installContext(cfg)
	if err != nil {
		_, _ = fmt.Fprintf(out, "workspace saved, but its docker context was not created: %v\n", err)
		return
	}
	_, _ = fmt.Fprintf(out, "docker context %q -> %s\n", installed.name, installed.endpoint)
}

// useContext makes a workspace's context the one docker resolves by default.
//
// Setting only OUR default is not enough. That is what `remote-docker
// docker ...` reads, but everything else on the machine reads docker's current
// context, so compose and the rest would keep talking to whatever was selected
// before -- usually a Docker Desktop pipe that is not there.
//
// Reported rather than fatal, for the same reason as reportContext: the
// workspace default has already been saved and is the part that was asked for.
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
		// Said out loud rather than passed over in silence. A context that is
		// not ours is the expected case for a name the user created
		// themselves, but it is also what a marker that failed to be written
		// looks like, and the difference matters, because in the second case
		// a context we made is left behind with nothing reporting it.
		_, _ = fmt.Fprintf(out, "docker context %q was left in place: "+
			"it is not marked as one remote-docker created\n", name)
		return
	}
	// Select default first: removing the active context would leave the CLI
	// pointing at one that no longer exists.
	_ = dockerCmd("context", "use", "default").Run()

	// CombinedOutput, not Run: an exit status alone says a removal failed
	// without saying why, and docker's own message is the whole diagnosis.
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
				return fmt.Errorf("no workspace is configured; add one with `%s`",
					ourCommand("create <name> --host <host>"))
			}

			out := cmd.OutOrStdout()
			row(out, "name", cfg.Name)

			// The transport rather than the raw host: a setting that decides
			// how a workspace is reached should not have to be worked out from
			// a string, and one that weakens something should not be
			// discoverable only by reading the JSON.
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
