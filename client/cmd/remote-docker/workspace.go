package main

import (
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/lhns/remote-docker/client/internal/config"
	"github.com/lhns/remote-docker/client/internal/proxy"
	"github.com/lhns/remote-docker/core/workspace"
)

// The workspaces in ~/.remote-docker.json, each with a docker context written
// as a side effect. Reached as `remote create` etc. (ADR 0024); the code keeps
// the noun "workspace", as the config, wire protocol and agent do.
func newWorkspaceCreateCommand() *cobra.Command {
	var flags workspaceFlags
	var makeDefault, noContext bool

	cmd := &cobra.Command{
		Use:     "create <name>",
		Aliases: []string{"add"},
		Short:   "Create a workspace and its docker context",
		Long: `Adds a workspace to this machine's configuration and creates a docker
context for it.

Given a name that already exists, it replaces every setting of that workspace.
"remote set" changes only the settings you name, and is the only way to change
a workspace made by "remote machine create".`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if flags.host == "" {
				return fmt.Errorf("--host is required\n  fix: `%s`", ourCommand("create "+name+" --host <host>"))
			}

			file, err := config.Load("")
			if err != nil {
				return err
			}
			old, existed := file.Workspaces[name]
			if m := old.Machine; m != nil {
				// Replacing the entry drops the only record of the machine, and
				// `rm` would then leave it running.
				return fmt.Errorf("workspace %q runs on the %s machine %q, which create would lose track of\n  fix: `%s` changes its settings",
					name, m.Backend, m.Name, ourCommand("set "+name+" ..."))
			}

			var ws config.Workspace
			flags.apply(&ws)
			if err := file.Set(name, ws); err != nil {
				return err
			}
			if err := checkWorkspace(file, name); err != nil {
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

	flags.register(cmd, "workspace address (required): a host, or ssh://, ws:// or wss:// with one")
	cmd.Flags().BoolVar(&makeDefault, "default", false, "make this the default workspace")
	cmd.Flags().BoolVar(&noContext, "no-context", false, "do not create a docker context")
	return cmd
}

func newWorkspaceSetCommand() *cobra.Command {
	var flags workspaceFlags

	cmd := &cobra.Command{
		Use:   "set <name>",
		Short: "Change some of a workspace's settings",
		Long: `Changes the settings named on the command line and keeps every other one.

Changing --host without --port goes back to the default port: 2222 for a bare
host, or the scheme's own for an ssh://, ws:// or wss:// address.

A workspace made by "remote machine create" keeps its machine. Its address is
found each time it connects, so --host is refused for it.

A running session keeps its old settings until it is restarted, and a new
--endpoint is refused while it runs.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if !flags.anyGiven() {
				return fmt.Errorf("no setting given to change\n  fix: `%s` lists them", ourCommand("set --help"))
			}
			file, err := config.Load("")
			if err != nil {
				return err
			}
			ws, ok := file.Workspaces[name]
			if !ok {
				return noWorkspaceNamed(name)
			}
			if m := ws.Machine; m != nil && flags.given("host") {
				return fmt.Errorf("workspace %q runs on the %s machine %q, whose address is found when it connects\n  fix: leave out --host",
					name, m.Backend, m.Name)
			}

			// Read before the change: a running session is on the old endpoint.
			before, err := resolve(args)
			if err != nil {
				return err
			}
			running := proxy.Reachable(endpointOf(before))

			flags.apply(&ws)
			if err := file.Set(name, ws); err != nil {
				return err
			}
			if err := checkWorkspace(file, name); err != nil {
				return err
			}
			next := before
			next.Endpoint = ws.Endpoint
			moved := flags.given("endpoint") && endpointOf(next) != endpointOf(before)
			if moved && running {
				// `restart` would look for it at the new endpoint.
				return fmt.Errorf("%s serves at %s, and moving the endpoint would leave it running there\n  fix: `%s` first",
					sessionOf(before), dockerHostOf(before), ourCommand("stop "+name))
			}
			if err := config.Save(file, ""); err != nil {
				return err
			}
			after, err := config.Resolve(config.Overrides{Workspace: name}, "")
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "updated workspace %q: %s\n", name, where(after))
			if moved && contextIsOurs(after.ContextName()) {
				reportContext(out, after)
			}
			if running {
				_, _ = fmt.Fprintf(out, "%s still uses the old settings\n  fix: `%s`\n",
					sessionOf(before), ourCommand("restart "+name))
			}
			return nil
		},
	}

	flags.register(cmd, "workspace address: a host, or ssh://, ws:// or wss:// with one")
	return cmd
}

// workspaceFlags are the settings create and set share.
type workspaceFlags struct {
	host, user, endpoint, watch, consistency, caFile string
	port                                             int
	insecure                                         bool

	set *pflag.FlagSet
}

func (f *workspaceFlags) register(cmd *cobra.Command, hostUsage string) {
	f.set = pflag.NewFlagSet("workspace", pflag.ContinueOnError)
	f.set.StringVar(&f.host, "host", "", hostUsage)
	f.set.StringVar(&f.caFile, "ca-file", "",
		"verify a ws:// endpoint against this CA instead of the system roots")
	f.set.BoolVar(&f.insecure, "insecure", false,
		"accept any certificate from a ws:// endpoint; ssh still authenticates both ends")
	f.set.IntVar(&f.port, "port", 0, "ssh port")
	f.set.StringVar(&f.user, "user", "", "workspace account; defaults to your local username")
	f.set.StringVar(&f.endpoint, "endpoint", "", "override where the local Docker API is served")
	f.set.StringVar(&f.watch, "watch", "", "replay file changes: off, partial or coarse")
	f.set.StringVar(&f.consistency, "consistency", "",
		"how shares are mounted: read=<direct|cached>,write=<through|back|ephemeral>")
	cmd.Flags().AddFlagSet(f.set)
}

// given reports whether a flag of ours was on the command line. The *Flag is
// shared with the command's set, which is the one that parses.
func (f *workspaceFlags) given(name string) bool { return f.set.Lookup(name).Changed }

func (f *workspaceFlags) anyGiven() bool {
	found := false
	f.set.VisitAll(func(fl *pflag.Flag) { found = found || fl.Changed })
	return found
}

// apply copies the flags given into ws and leaves every other field.
func (f *workspaceFlags) apply(ws *config.Workspace) {
	given := f.given
	if given("host") && f.host != ws.Host {
		ws.Host = f.host
		ws.Port = defaultPort(f.host)
	}
	if given("port") {
		ws.Port = f.port
	}
	if given("user") {
		ws.User = f.user
	}
	if given("endpoint") {
		ws.Endpoint = f.endpoint
	}
	if given("watch") {
		ws.Watch = f.watch
	}
	if given("consistency") {
		ws.Consistency = f.consistency
	}
	if given("ca-file") {
		ws.CAFile = f.caFile
	}
	if given("insecure") {
		ws.Insecure = f.insecure
	}
}

// defaultPort is the port a new host starts with. Only a bare host gets one:
// Transport refuses a port beside a URL that disagrees with it.
func defaultPort(host string) int {
	if strings.Contains(host, "://") {
		return 0
	}
	return config.DefaultSSHPort
}

// checkWorkspace refuses an entry that would otherwise fail only on the first
// docker command.
func checkWorkspace(file config.File, name string) error {
	if _, err := workspace.ParseMode(file.Workspaces[name].Consistency); err != nil {
		return err
	}
	_, err := file.Transport(name)
	return err
}

func newWorkspaceRemoveCommand() *cobra.Command {
	var keepContext, keepMachine, force bool

	cmd := &cobra.Command{
		Use:     "rm <name>",
		Aliases: []string{"remove"},
		Short:   "Remove a workspace and its docker context",
		Long: `Stops the workspace's background session, then removes the workspace from
this machine's configuration and the docker context remote-docker created for
it. Refused while that session is in use; -f overrides.

A workspace made by "remote machine create" has its machine destroyed too,
with the images, containers and volumes inside it. Your files are not in it.
--keep-machine leaves the machine running.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			file, err := config.Load("")
			if err != nil {
				return err
			}
			ws, ok := file.Workspaces[name]
			if !ok {
				return noWorkspaceNamed(name)
			}

			// Before removal: the context name derives from the entry.
			cfg, cfgErr := resolve(args)

			// Destroyed before the entry goes: the entry is the only record the
			// machine exists, so a failure must leave it to retry from.
			machine := ws.Machine
			if cfgErr == nil {
				consequence := "removing it takes its file server away"
				if machine != nil && !keepMachine {
					consequence = "removing it destroys its machine"
				}
				if err := refuseInUse(cfg, force, consequence, "rm"); err != nil {
					return err
				}
			}
			// As `machine stop` does: a session left serving answers for a
			// workspace that is gone.
			stopSessionFor(cmd, name)
			if machine != nil && !keepMachine {
				if err := destroyMachine(cmd, machine); err != nil {
					return err
				}
			}

			file.Remove(name)
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
	forceFlag(cmd, &force, "remove even if the session is in use")
	return cmd
}

// destroyMachine destroys a workspace's machine. An unknown backend is an
// error, so `rm` refuses rather than orphaning a running machine.
func destroyMachine(cmd *cobra.Command, m *config.Machine) error {
	backend, err := findBackend(m.Backend)
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
				return noWorkspaceNamed(name)
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

			rows := make([]listRow, 0, len(names))
			for _, name := range names {
				marker := " "
				if name == file.Default {
					marker = "*"
				}
				row := listRow{name: marker + name}
				cfg, err := config.Resolve(config.Overrides{Workspace: name}, "")
				if err != nil {
					row.err = err
					rows = append(rows, row)
					continue
				}
				row.where = where(cfg)
				if m := file.Workspaces[name].Machine; m != nil {
					// Shown because `rm` destroys it.
					row.where += " (" + m.Backend + ")"
				}
				row.endpoint = dockerHostOf(cfg)
				rows = append(rows, row)
			}
			printWorkspaces(out, rows)
			_, _ = fmt.Fprintln(out, "\n* default")
			return nil
		},
	}
}

// listRow is one line of `remote ls`. name carries the default marker.
type listRow struct {
	name, where, endpoint string
	err                   error
}

// printWorkspaces writes the table with each column as wide as its widest
// entry. A row that could not be resolved prints its error where the
// workspace would be, and does not widen that column.
func printWorkspaces(out io.Writer, rows []listRow) {
	nameW, whereW := len("NAME"), len("WORKSPACE")
	for _, r := range rows {
		nameW = max(nameW, utf8.RuneCountInString(r.name))
		if r.err == nil {
			whereW = max(whereW, utf8.RuneCountInString(r.where))
		}
	}
	_, _ = fmt.Fprintf(out, "%-*s   %-*s   %s\n", nameW, "NAME", whereW, "WORKSPACE", "ENDPOINT")
	for _, r := range rows {
		if r.err != nil {
			_, _ = fmt.Fprintf(out, "%-*s   %v\n", nameW, r.name, r.err)
			continue
		}
		_, _ = fmt.Fprintf(out, "%-*s   %-*s   %s\n", nameW, r.name, whereW, r.where, r.endpoint)
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
	if err := installContext(cfg); err != nil {
		_, _ = fmt.Fprintf(out, "workspace saved, but its docker context was not created: %v\n", err)
		return
	}
	_, _ = fmt.Fprintf(out, "docker context %q -> %s\n", cfg.ContextName(), dockerHostOf(cfg))
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
	if key, err := enrolledPublicKey(); err == nil {
		return key
	}
	return "(run `" + ourCommand("enroll") + "` to generate one)"
}

// newWorkspaceInspectCommand shows everything about one workspace in one place.
func newWorkspaceInspectCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "inspect [name]",
		Short: "Show a workspace's settings, endpoint and docker context",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve(args)
			if err != nil {
				return err
			}
			if err := requireHost(cfg); err != nil {
				return err
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
