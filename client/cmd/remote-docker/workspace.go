package main

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/lhns/remote-docker/client/internal/config"
	"github.com/lhns/remote-docker/client/internal/proxy"
	"github.com/lhns/remote-docker/client/internal/session"
	"github.com/lhns/remote-docker/core/enrol"
	"github.com/lhns/remote-docker/core/workspace"
)

// The workspaces in ~/.remote-docker.json, each with a docker context written
// as a side effect. Reached as `remote create` etc. (ADR 0024); the code keeps
// the noun "workspace", as the config, wire protocol and agent do.
func newWorkspaceCreateCommand() *cobra.Command {
	var flags workspaceFlags
	var makeDefault, noContext bool
	var token string

	cmd := &cobra.Command{
		Use:     "create <name>",
		Aliases: []string{"add"},
		Short:   "Create a workspace and its docker context",
		Long: `Adds a workspace and its docker context. For a name that exists, every
setting is replaced; "remote set" changes only the ones you name.

With --token, the invite an operator or admin gave you enrols this machine's
key first, and nothing is saved unless it does. The invite carries the
workspace's address, which --host overrides, and its host key.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			var invite enrol.Invite
			if cmd.Flags().Changed("token") {
				var err error
				if invite, err = enrol.ParseInvite(token); err != nil {
					return fmt.Errorf("--token is not an enrolment invite, which starts %s\n  fix: paste the whole invite you were given", enrol.InvitePrefix)
				}
				if !flags.given("host") {
					_ = flags.set.Set("host", invite.URL)
				}
			}
			if flags.host == "" {
				return fmt.Errorf("--host is required\n  fix: `%s`", ourCommand("create "+name+" --host <host>"))
			}

			file, err := config.Load("")
			if err != nil {
				return err
			}
			old, existed := file.Workspaces[name]
			// The entry is the only record of the machine (ADR 0026).
			if m := old.Machine; m != nil {
				return fmt.Errorf("workspace %q is the %s machine %q, which replacing it would forget\n  fix: `%s` changes its settings; `%s` first replaces it",
					name, m.Backend, m.Name, ourCommand("set "+name), ourCommand("rm "+name))
			}

			var ws config.Workspace
			flags.apply(&ws)
			if err := file.Set(name, ws); err != nil {
				return err
			}
			if makeDefault {
				file.Default = name
			}
			cfg, err := resolveChecked(file, name)
			if err != nil {
				return err
			}
			var enrolled *enrol.RedeemReply
			if invite.Token != "" {
				reply, err := redeem(cmd, cfg, invite, flags)
				if err != nil {
					return err
				}
				enrolled = &reply
				ws.User = reply.Account
				if err := file.Set(name, ws); err != nil {
					return err
				}
				if cfg, err = resolveChecked(file, name); err != nil {
					return err
				}
			}
			if err := config.Save(file, ""); err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			verb := "added"
			if existed {
				verb = "updated"
			}
			_, _ = fmt.Fprintf(out, "%s workspace %q: %s\n", verb, name, where(cfg))

			if enrolled != nil {
				how := "joined the account"
				if enrolled.Created {
					how = "created the account"
				}
				_, _ = fmt.Fprintf(out, "this machine's key %s %s\n", how, enrolled.Account)
				if enrolled.Pending {
					_, _ = fmt.Fprintf(out, "the account %s is still being created on the workspace, which refuses this key until it is\n"+
						"  fix: wait a minute or two before the first docker command\n", enrolled.Account)
				}
			}
			if !noContext {
				reportContext(out, cfg)
			}

			if enrolled == nil {
				_, _ = fmt.Fprintf(out,
					"\nIf this machine is not enrolled there yet, hand this to whoever runs it:\n\n    %s\n\n"+
						"With an enrolment token instead, nobody has to save it: `%s`\n",
					enrolledKey(), ourCommand("create "+name+" --token <invite>"))
			}
			return nil
		},
	}

	flags.register(cmd, "workspace address (required without --token): a host, or ssh://, ws:// or wss:// with one")
	cmd.Flags().StringVar(&token, "token", "", "enrol this machine's key with an invite first")
	cmd.Flags().BoolVar(&makeDefault, "default", false, "make this the default workspace")
	cmd.Flags().BoolVar(&noContext, "no-context", false, "do not create a docker context")
	return cmd
}

func newWorkspaceSetCommand() *cobra.Command {
	var flags workspaceFlags

	cmd := &cobra.Command{
		Use:   "set <name>",
		Short: "Change some of a workspace's settings",
		Long: `Changes the settings you name and keeps the rest. A new --host without
--port goes back to the default port. A running session keeps the old settings
until "remote restart".`,
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
			if err := flags.refuseForMachine(name, ws.Machine); err != nil {
				return err
			}

			// An entry that does not resolve has no session, and set is how it
			// gets repaired.
			before, err := file.Resolve(config.Overrides{Workspace: name})
			running := err == nil && proxy.Reachable(endpointOf(before))

			flags.apply(&ws)
			file.Workspaces[name] = ws
			after, err := resolveChecked(file, name)
			if err != nil {
				return err
			}
			moved := endpointOf(after) != endpointOf(before)
			if moved && running {
				// `restart` would look for it at the new endpoint.
				return fmt.Errorf("%s serves at %s, and moving the endpoint would leave it running there\n  fix: `%s` first",
					sessionOf(before), dockerHostOf(before), ourCommand("stop "+name))
			}
			if err := config.Save(file, ""); err != nil {
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

	// Ours alone, so anyGiven ignores the inherited --workspace.
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
		ws.Host, ws.Port = f.host, 0
		// Only a bare host: Transport refuses a port beside a URL that disagrees.
		if !strings.Contains(f.host, "://") {
			ws.Port = config.DefaultSSHPort
		}
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

// refuseForMachine refuses what a machine decides itself: its address is found
// at every connect, and its port and account are built into it.
func (f *workspaceFlags) refuseForMachine(name string, m *config.Machine) error {
	if m == nil {
		return nil
	}
	if f.given("host") {
		return fmt.Errorf("workspace %q is the %s machine %q, whose address is found when it connects\n  fix: leave out --host",
			name, m.Backend, m.Name)
	}
	if !f.given("port") && !f.given("user") {
		return nil
	}
	rebuild := "machine rebuild " + name
	if f.given("port") {
		rebuild += " --port " + strconv.Itoa(f.port)
	}
	if f.given("user") {
		rebuild += " --user " + f.user
	}
	return fmt.Errorf("workspace %q is the %s machine %q, which was built for its port and user\n  fix: `%s`, which discards its containers",
		name, m.Backend, m.Name, ourCommand(rebuild))
}

// redeem enrols this machine's key with an invite, as the account --user
// names, or the one the token is bound to, or this machine's user name for a
// token that makes a new account.
func redeem(cmd *cobra.Command, cfg config.Config, invite enrol.Invite, flags workspaceFlags) (enrol.RedeemReply, error) {
	account := ""
	if flags.given("user") {
		account = flags.user
	} else if invite.Account == "" {
		account = cfg.User
	}
	reply, err := session.Redeem(cmd.Context(), cfg, invite, account)
	return reply, redeemError(err, flags.host)
}

// redeemError words a failed redemption for the person at this machine.
func redeemError(err error, host string) error {
	if errors.Is(err, session.ErrPredatesTokens) {
		return fmt.Errorf("the workspace at %s predates enrolment tokens\n  fix: ask its operator to upgrade it, or to enrol this key by file: `%s`",
			host, ourCommand("enroll"))
	}
	return err
}

// resolveChecked resolves the named workspace in file, refusing what would
// otherwise fail only on the first docker command.
func resolveChecked(file config.File, name string) (config.Config, error) {
	cfg, err := file.Resolve(config.Overrides{Workspace: name})
	if err != nil {
		return cfg, err
	}
	if _, err := workspace.ParseMode(cfg.Consistency); err != nil {
		return cfg, err
	}
	_, err = cfg.Transport()
	return cfg, err
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
				rowf(out, "workspace", "%s@%s (%s)", cfg.User, cfg.Host, firstLine(err.Error()))
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
