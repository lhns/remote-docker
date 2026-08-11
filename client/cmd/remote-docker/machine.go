package main

// `remote machine`: the Linux system a workspace runs on, when this machine
// has none.
//
// What these commands produce is an ordinary workspace (ADR 0026). `remote ls`
// lists it, `remote use` selects it, `docker run` reaches it through the same
// session, the same NFS export and the same rewriting as a host in another
// country. The only command that knows the difference is `rm`, which has a
// machine to destroy as well as an entry to delete.

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/lhns/remote-docker/client/internal/config"
	"github.com/lhns/remote-docker/client/internal/machine"
	"github.com/lhns/remote-docker/client/internal/proxy"
	"github.com/lhns/remote-docker/client/internal/sshx"
)

func newMachineCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "machine",
		Short: "Create and manage the local Linux system a workspace runs on",
		Long: `Provisions a Linux system on this machine and registers it as a workspace.

What comes out is an ordinary workspace: "remote ls" lists it and docker
commands reach it exactly as they reach one on another host. These commands are
its lifecycle, and nothing else treats it specially.

Nothing is installed. The machine is the workspace image's filesystem, and
changing versions replaces it rather than upgrading it, so there is no
half-finished state to be in. "rebuild" is the ordinary path run again, and it
discards what is inside the machine: images and containers, never your files,
which live here and are served to it.`,
		Args: onlySubcommands,
		RunE: helpWhenBare,
	}
	cmd.AddCommand(
		newMachineCreateCommand(),
		newMachineStartCommand(),
		newMachineStopCommand(),
		newMachineRebuildCommand(),
		newMachineStatusCommand(),
	)
	return cmd
}

// machineOptions are the settings a machine is built from, shared by create
// and rebuild so the two cannot drift into building different things.
type machineOptions struct {
	backend  string
	rootfs   string
	cpus     int
	memoryMB int
}

func (o *machineOptions) install(cmd *cobra.Command) {
	cmd.Flags().StringVar(&o.backend, "backend", "wsl",
		"wsl, or hyperv (never executed by anybody -- see docs/testing-machines.md)")
	cmd.Flags().StringVar(&o.rootfs, "rootfs", "",
		"what the machine is built from: the workspace image's filesystem as a tar file (wsl), or a Flatcar disk image (hyperv)")
	cmd.Flags().IntVar(&o.cpus, "cpus", 0, "processors to give it; 0 uses the backend's default")
	cmd.Flags().IntVar(&o.memoryMB, "memory", 0, "megabytes to give it; 0 uses the backend's default")

	// No --port or --user here. `remote` already carries both persistently and
	// they mean exactly this, so declaring them again would put two flags of
	// the same name on one command line -- where pflag silently skips the
	// duplicate and which one wins depends on where it was declared. That is
	// the hazard ADR 0024 moved our flags off the root to avoid, and it would
	// be a poor thing to reintroduce two levels down.
}

// spec turns the flags and a name into what the backend is asked to build.
//
// The port and the account come from `remote`'s own flags, which is where they
// already live, falling back to the same defaults the resolver uses so that a
// machine and a hand-written workspace agree about what "unset" means.
func (o *machineOptions) spec(name string) machine.Spec {
	account := overrides.User
	if account == "" {
		account = config.DefaultUser()
	}
	port := overrides.Port
	if port == 0 {
		port = config.DefaultSSHPort
	}
	return machine.Spec{
		Name:     name,
		Backend:  o.backend,
		Rootfs:   o.rootfs,
		Port:     port,
		CPUs:     o.cpus,
		MemoryMB: o.memoryMB,
		Account:  account,
	}
}

func newMachineCreateCommand() *cobra.Command {
	var opts machineOptions

	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a machine and register it as a workspace",
		Long: `Creates the Linux system, enrols this machine's key in it, and writes the
workspace and its docker context.

Idempotent: run against a machine that already matches, it says so and does
nothing. Run against one built from different settings, it reports the mismatch
rather than acting on it, because recreating discards what is inside and that
is not a thing a create command should decide.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return createMachine(cmd, args[0], opts.spec(args[0]), false)
		},
	}
	opts.install(cmd)
	return cmd
}

func newMachineRebuildCommand() *cobra.Command {
	var opts machineOptions

	cmd := &cobra.Command{
		Use:   "rebuild <name>",
		Short: "Destroy and recreate the machine",
		Long: `Destroys the machine and builds it again from the same settings.

This is the repair path, and it is the ordinary path run again rather than a
special mode: the machine is defined entirely by its configuration, so there is
nothing to repair in place.

Images, containers and volumes INSIDE the machine are lost. Your files are not:
they are on this machine and are served to it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return createMachine(cmd, args[0], opts.spec(args[0]), true)
		},
	}
	opts.install(cmd)
	return cmd
}

// stopSessionFor shuts down the background session for a workspace, if one is
// serving.
//
// Best effort by design: this runs before stopping a machine, and every way it
// can fail -- no config, nothing listening, a session that will not answer --
// means the same thing to the caller, which is that there is nothing to shut
// down first. A machine that would not stop is what the caller is told about,
// and that error comes from the stop itself.
func stopSessionFor(cmd *cobra.Command, name string) {
	cfg, err := config.Resolve(overrides, name)
	if err != nil {
		return
	}
	endpoint := endpointOf(cfg)
	if !proxy.Reachable(endpoint) {
		return
	}

	if err := control(endpoint, http.MethodPost, "shutdown", nil); err != nil {
		return
	}
	_ = waitForEndpoint(endpoint, false, stopTimeout)
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), "stopped the session using it")
}

// unproven names the backends that have never been executed.
//
// Not a capability check: hyperv COMPILES, is unit tested as far as a string
// can be, and may well work. What it has never done is run, and that is a
// different claim from "unavailable" -- which is why it is a warning and not a
// refusal. See CLAUDE.md's NOT-tested list, which this must agree with.
var unproven = map[string]bool{"hyperv": true}

// createMachine is the whole of create and rebuild, which differ only in
// whether they are allowed to destroy what is there.
func createMachine(cmd *cobra.Command, name string, spec machine.Spec, rebuild bool) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	backend, err := machine.Find(spec.Backend)
	if err != nil {
		return err
	}
	if err := backend.Available(ctx); err != nil {
		return err
	}

	// Said out loud, every time, by the program itself. A backend nobody has
	// ever run is not the same kind of thing as one CI proves on every change,
	// and a flag list that spells them the same way is the one place somebody
	// choosing between them actually looks. It stays until somebody has run
	// docs/testing-machines.md and said what happened.
	if unproven[spec.Backend] {
		_, _ = fmt.Fprintf(out, "warning: the %s backend has never been run by anybody\n"+
			"  fix: docs/testing-machines.md is its only verification, and a report of what happens is worth more than a patch\n",
			spec.Backend)
	}

	// Read before anything is built. One backend needs it at creation and the
	// other writes it afterwards, and failing on a missing key after building a
	// machine would leave one nobody can reach.
	key, err := enrolledPublicKey()
	if err != nil {
		return err
	}
	spec.PublicKey = key

	observed, err := backend.Inspect(ctx, name)
	if err != nil {
		return fmt.Errorf("cannot tell what is there: %w", err)
	}

	switch action := machine.Plan(spec, observed); {
	case rebuild && observed.State != machine.Absent:
		_, _ = fmt.Fprintf(out, "destroying %q; images and containers inside it are lost\n", name)
		if err := backend.Destroy(ctx, name); err != nil {
			return fmt.Errorf("destroying %s: %w", name, err)
		}
		fallthrough

	case action == machine.Create:
		_, _ = fmt.Fprintf(out, "creating the %s machine %q\n", spec.Backend, spec.Name)
		if err := backend.Create(ctx, spec); err != nil {
			return fmt.Errorf("creating %s: %w", name, err)
		}

	case action == machine.Start:
		_, _ = fmt.Fprintf(out, "%q exists and matches; starting it\n", name)
		if err := backend.Start(ctx, name); err != nil {
			return fmt.Errorf("starting %s: %w", name, err)
		}

	case action == machine.Recreate:
		// Reported, not acted on. Recreating discards everything inside, and a
		// create command deciding that on somebody's behalf is the surprise
		// this whole design exists to avoid.
		return fmt.Errorf("%q was built from different settings\n"+
			"  fix: `%s` to destroy and rebuild it, which discards its images and containers",
			name, ourCommand("machine rebuild "+name))

	default:
		_, _ = fmt.Fprintf(out, "%q already matches; nothing to do\n", name)
	}

	// Waited for, because "created" has to mean "usable". The agent has to
	// start, generate a host key and open its listener, and returning before
	// that hands the user a workspace whose first command fails with a refused
	// connection.
	//
	// Held open for the whole wait. Without this the machine shuts down about
	// thirty seconds in, restarts on the next poke, and its dockerd never gets
	// far enough to be ready -- so the agent never listens and the wait times
	// out against a machine that keeps starting from scratch.
	hold, err := machine.Hold(ctx, spec.Backend, name)
	if err != nil {
		return err
	}
	defer func() { _ = hold.Close() }()

	// Located, not assumed. The machine answers at its own address on a virtual
	// network, and not on this computer's loopback: with WSL that would depend
	// on its localhost relay, which was measured refusing the connection while
	// the machine was running and its agent listening (2026-08-11, the `a
	// machine on wsl` job).
	host, err := machine.Locate(ctx, spec.Backend, name)
	if err != nil {
		return err
	}

	if err := waitForAgent(ctx, host, spec.Port); err != nil {
		return err
	}

	// Enrolled every time, including when nothing else happened: it is how a
	// rotated key reaches an existing machine without a rebuild. On a backend
	// where that is impossible it reports rather than writes.
	if err := backend.Enrol(ctx, name, spec.Account, key); err != nil {
		return fmt.Errorf("enrolling this machine's key: %w", err)
	}

	return saveMachineWorkspace(cmd, name, spec)
}

// waitForAgent blocks until the machine's agent accepts a connection.
//
// A dial rather than a handshake: this is asking whether the listener is open,
// and anything further is the session's job to report properly. The timeout is
// generous because a machine's first start does more than a later one.
func waitForAgent(ctx context.Context, host string, port int) error {
	addr := net.JoinHostPort(host, fmt.Sprint(port))
	deadline := time.Now().Add(agentStartTimeout)

	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("the machine was created but its agent is not answering on %s\n"+
		"  fix: `%s` shows what state it is in", addr, ourCommand("machine status"))
}

// agentStartTimeout is how long the agent has to open its listener.
//
// Longer than the agent's own wait for dockerd, deliberately. It gives the
// daemon ninety seconds and then serves anyway, on the argument that a
// workspace somebody can log into beats one that took the evidence with it --
// so a client that waits ninety seconds gives up at the exact moment the agent
// would have started answering, and reports a machine that was about to work.
const agentStartTimeout = 3 * time.Minute

// machinePlaceholderHost stands in for an address nobody should read. See
// saveMachineWorkspace.
const machinePlaceholderHost = "127.0.0.1"

// saveMachineWorkspace writes the workspace entry, which is what makes the
// machine an ordinary workspace everywhere else.
func saveMachineWorkspace(cmd *cobra.Command, name string, spec machine.Spec) error {
	file, err := config.Load("")
	if err != nil {
		return err
	}

	ws := file.Workspaces[name]
	// A placeholder, and only that. A machine's address is asked for at every
	// connection (session.connect), because it is given out at boot and a
	// stored one is wrong the moment the machine restarts. It is written at all
	// so the entry is a complete workspace to everything that reads one.
	ws.Host = machinePlaceholderHost
	ws.Port = spec.Port
	ws.User = spec.Account
	ws.Machine = &config.Machine{
		Backend:    spec.Backend,
		Name:       spec.Name,
		Image:      spec.Image,
		Rootfs:     spec.Rootfs,
		CPUs:       spec.CPUs,
		MemoryMB:   spec.MemoryMB,
		Generation: spec.Generation(),
	}
	if err := file.Set(name, ws); err != nil {
		return err
	}
	if file.Default == "" {
		file.Default = name
	}
	if err := config.Save(file, ""); err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "workspace %q -> %s@127.0.0.1:%d\n", name, spec.Account, spec.Port)

	cfg, err := config.Resolve(config.Overrides{Workspace: name}, "")
	if err == nil {
		reportContext(out, cfg)
	}
	_, _ = fmt.Fprintf(out, "\nTry `%s`.\n", programName()+" run --rm -v .:/w alpine ls /w")
	return nil
}

// enrolledPublicKey is this machine's public half, generating the pair if this
// is the first thing that has needed it.
func enrolledPublicKey() (string, error) {
	key, err := sshx.LoadOrCreateKey(config.KeyPath(), config.KeyComment())
	if err != nil {
		return "", err
	}
	return key.AuthorizedKey(config.KeyComment()), nil
}

func newMachineStartCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "start <name>",
		Short: "Start the machine",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withMachine(cmd, args[0], func(ctx context.Context, _ machine.Backend, ws config.Workspace) error {
				// Waited for, for the same reason create waits: "started" has
				// to mean "usable". Starting a machine and returning leaves its
				// agent still generating a host key and opening a listener, so
				// whatever runs next races it and loses.
				//
				// Held while waiting, because a WSL machine nobody is in shuts
				// down again -- a start that allowed that would be a command
				// which reliably undid itself.
				hold, err := machine.Hold(ctx, ws.Machine.Backend, ws.Machine.Name)
				if err != nil {
					return err
				}
				defer func() { _ = hold.Close() }()

				host, err := machine.Locate(ctx, ws.Machine.Backend, ws.Machine.Name)
				if err != nil {
					return err
				}
				if err := waitForAgent(ctx, host, ws.Port); err != nil {
					return err
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "started %q\n", ws.Machine.Name)
				return nil
			})
		},
	}
}

func newMachineStopCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "stop <name>",
		Short: "Stop the machine",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withMachine(cmd, args[0], func(ctx context.Context, b machine.Backend, ws config.Workspace) error {
				m := ws.Machine
				// The session goes first. It is holding this machine open and
				// serving a Docker API backed by it, so stopping the machine
				// underneath leaves a session answering for something that is
				// gone -- which presents as the NEXT command failing with EOF
				// on a local pipe, naming nothing that suggests a machine.
				stopSessionFor(cmd, args[0])

				if err := b.Stop(ctx, m.Name); err != nil {
					return err
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "stopped %q\n", m.Name)
				return nil
			})
		},
	}
}

func newMachineStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status <name>",
		Short: "Is the machine there, running, and built from the current settings?",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withMachine(cmd, args[0], func(ctx context.Context, b machine.Backend, ws config.Workspace) error {
				m := ws.Machine
				observed, err := b.Inspect(ctx, m.Name)
				if err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				row(out, "machine", fmt.Sprintf("%s (%s)", m.Name, m.Backend))
				row(out, "state", observed.State.String())
				reportGeneration(out, m, observed)
				return nil
			})
		},
	}
}

// reportGeneration says whether the machine matches the settings it would be
// built from now, which is the question `status` is really asked.
func reportGeneration(out io.Writer, m *config.Machine, observed mObserved) {
	switch observed.Generation {
	case "":
		row(out, "settings", "cannot be read from the machine")
	case m.Generation:
		row(out, "settings", "current")
	default:
		rowf(out, "settings", "built from older ones (run `%s`)",
			ourCommand("machine rebuild "+m.Name))
	}
}

// mObserved is machine.Observed, aliased so the signature above reads without
// a package qualifier in every line.
type mObserved = machine.Observed

// withMachine looks up a workspace's machine and hands it to fn.
func withMachine(cmd *cobra.Command, name string, fn func(context.Context, machine.Backend, config.Workspace) error) error {
	file, err := config.Load("")
	if err != nil {
		return err
	}
	ws, ok := file.Workspaces[name]
	if !ok {
		return fmt.Errorf("no workspace named %q; `%s` shows what there is", name, ourCommand("ls"))
	}
	if ws.Machine == nil {
		return fmt.Errorf("workspace %q is not backed by a machine this program built\n"+
			"  fix: these commands manage machines created with `%s`",
			name, ourCommand("machine create <name>"))
	}
	backend, err := machine.Find(ws.Machine.Backend)
	if err != nil {
		return err
	}
	return fn(cmd.Context(), backend, ws)
}
