package main

// `remote machine`: provisions a local Linux system as an ordinary workspace
// (ADR 0026).

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/lhns/remote-docker/client/internal/config"
	"github.com/lhns/remote-docker/client/internal/proxy"
	"github.com/lhns/remote-docker/core-client/keys"
	"github.com/lhns/remote-docker/machine"
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
which live here and are served to it.

"remote rm <name>" removes a machine along with its workspace.

Every command but create takes a workspace name, else --workspace, else the
default.`,
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
// and rebuild.
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
		"build from this file instead of the published one: the workspace image's filesystem as a tar (wsl), or a Flatcar disk image (hyperv)")
	cmd.Flags().IntVar(&o.cpus, "cpus", 0, "processors to give it; 0 uses the backend's default")
	cmd.Flags().IntVar(&o.memoryMB, "memory", 0, "megabytes to give it; 0 uses the backend's default")

	// No --port or --user: `remote` has both persistently, and pflag silently
	// skips a duplicate (ADR 0024).
}

// spec turns the flags and a name into what the backend is asked to build.
// Unset settings fall back to the recorded ones, since Spec.Generation hashes
// them all: otherwise rebuilding a `--port 2222` machine builds one on 22 that
// `status` calls out of date forever. The recorded rootfs is reused only for
// the same image and while the file still exists.
func (o *machineOptions) spec(name string, recorded *config.Workspace) machine.Spec {
	spec := machine.Spec{
		Name:    name,
		Backend: o.backend,
		// Part of the generation, so a new client version rebuilds (ADR 0026).
		Image:    machine.DefaultImage(version),
		Rootfs:   o.rootfs,
		Port:     overrides.Port,
		CPUs:     o.cpus,
		MemoryMB: o.memoryMB,
		Account:  overrides.User,
	}
	if recorded != nil && recorded.Machine != nil {
		m := recorded.Machine
		if spec.Port == 0 {
			spec.Port = recorded.Port
		}
		if spec.Account == "" {
			spec.Account = recorded.User
		}
		if spec.CPUs == 0 {
			spec.CPUs = m.CPUs
		}
		if spec.MemoryMB == 0 {
			spec.MemoryMB = m.MemoryMB
		}
		if spec.Rootfs == "" && m.Image == spec.Image && m.Rootfs != "" {
			if _, err := os.Stat(m.Rootfs); err == nil {
				spec.Rootfs = m.Rootfs
			}
		}
	}
	if spec.Port == 0 {
		spec.Port = config.DefaultSSHPort
	}
	if spec.Account == "" {
		spec.Account = config.DefaultUser()
	}
	return spec
}

// recordedWorkspace is the workspace entry a machine was registered under,
// which holds the settings it was built from, or nil when there is no entry or
// it names no machine.
func recordedWorkspace(name string) *config.Workspace {
	file, err := config.Load("")
	if err != nil {
		return nil
	}
	ws, ok := file.Workspaces[name]
	if !ok || ws.Machine == nil {
		return nil
	}
	return &ws
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
		// Flags alone, not the record, or a mismatch could never show.
		RunE: func(cmd *cobra.Command, args []string) error {
			return createMachine(cmd, args[0], opts.spec(args[0], nil), false)
		},
	}
	opts.install(cmd)
	return cmd
}

func newMachineRebuildCommand() *cobra.Command {
	var opts machineOptions
	var force bool

	cmd := &cobra.Command{
		Use:   "rebuild [name]",
		Short: "Destroy and recreate the machine",
		Long: `Destroys the machine and builds it again from the same settings.

This is the repair path, and it is the ordinary path run again rather than a
special mode: the machine is defined entirely by its configuration, so there is
nothing to repair in place.

Images, containers and volumes INSIDE the machine are lost. Your files are not:
they are on this machine and are served to it. Refused while the workspace's
session is in use; -f overrides.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := machineName(args)
			if err != nil {
				return err
			}
			// A name nothing is configured under has no session to ask.
			if cfg, err := resolve([]string{name}); err == nil {
				if err := refuseInUse(cfg, force, "rebuilding destroys its machine", "machine rebuild"); err != nil {
					return err
				}
			}
			return createMachine(cmd, name, opts.spec(name, recordedWorkspace(name)), true)
		},
	}
	opts.install(cmd)
	forceFlag(cmd, &force, "rebuild even if the workspace's session is in use")
	return cmd
}

// machineName is the workspace a machine command is about: its [name], else
// --workspace, else the default. A name given is taken as it stands, so
// `rebuild` can still reach a machine whose workspace entry is gone.
func machineName(args []string) (string, error) {
	if len(args) > 0 {
		return args[0], nil
	}
	cfg, err := resolve(nil)
	if err != nil {
		return "", err
	}
	if cfg.Name == "" {
		return "", fmt.Errorf("%w\n  fix: `%s`", config.ErrNoWorkspace, ourCommand("machine create <name>"))
	}
	return cfg.Name, nil
}

// findBackend is machine.Find, replaced in tests.
var findBackend = machine.Find

// stopSessionFor stops the workspace's background session, if one is serving.
// Best effort, but every failure is reported: a surviving session makes the
// next docker command fail with a bare EOF over a dead connection.
func stopSessionFor(cmd *cobra.Command, name string) {
	warn := func(format string, args ...any) {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: "+format+"\n", args...)
	}

	cfg, err := resolve([]string{name})
	if err != nil {
		warn("cannot tell which endpoint %q uses, so a session may still be serving it: %v", name, err)
		return
	}
	endpoint := endpointOf(cfg)
	// Reachable is a plain dial, so a session that has bound its endpoint
	// answers it; `machine start` relies on that to close the race with `stop`.
	if !proxy.Reachable(endpoint) {
		return
	}
	if err := stopSession(endpoint); err != nil {
		warn("%v", err)
		return
	}
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), "stopped the session using it")
}

// unproven names the backends never executed, which warn rather than refuse.
// Must agree with CLAUDE.md's NOT-tested list.
var unproven = map[string]bool{"hyperv": true}

// createMachine is create and rebuild; only rebuild may destroy.
func createMachine(cmd *cobra.Command, name string, spec machine.Spec, rebuild bool) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	backend, err := findBackend(spec.Backend)
	if err != nil {
		return err
	}
	if err := backend.Available(ctx); err != nil {
		return err
	}

	if unproven[spec.Backend] {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: the %s backend has never been run by anybody\n"+
			"  fix: docs/testing-machines.md is its only verification, and a report of what happens is worth more than a patch\n",
			spec.Backend)
	}

	// Before building, so a missing key cannot leave an unreachable machine.
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
		stopSessionFor(cmd, name)
		if err := backend.Destroy(ctx, name); err != nil {
			return fmt.Errorf("destroying %s: %w", name, err)
		}
		fallthrough

	case action == machine.Create:
		// Fetched only when building: it is several hundred megabytes.
		if spec.Rootfs == "" {
			if spec.Rootfs, err = machine.EnsureRootfs(ctx, spec.Image, out); err != nil {
				return err
			}
		}

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
		// Reported, not acted on: recreating discards everything inside.
		return fmt.Errorf("%q was built from different settings\n"+
			"  fix: `%s` to destroy and rebuild it, which discards its images and containers",
			name, ourCommand("machine rebuild "+name))

	default:
		_, _ = fmt.Fprintf(out, "%q already matches; nothing to do\n", name)
	}

	// Waited for until the agent listens, so "created" means usable. Held open
	// meanwhile: an empty WSL machine shuts down after ~30s and never gets there.
	hold, err := machine.Hold(ctx, spec.Backend, name)
	if err != nil {
		return err
	}
	defer func() { _ = hold.Close() }()

	// At its own address, not loopback: WSL's localhost relay was measured
	// refusing connections to a listening agent (machine.yml, 2026-08-11).
	if _, err := machine.Locate(ctx, spec.Backend, name, spec.Port); err != nil {
		return err
	}

	// Every time, so a rotated key reaches an existing machine.
	if err := backend.Enrol(ctx, name, spec.Account, key); err != nil {
		return fmt.Errorf("enrolling this machine's key: %w", err)
	}

	return saveMachineWorkspace(cmd, name, spec)
}

// machinePlaceholderHost stands in for an address nobody should read.
const machinePlaceholderHost = "127.0.0.1"

// saveMachineWorkspace writes the workspace entry.
func saveMachineWorkspace(cmd *cobra.Command, name string, spec machine.Spec) error {
	file, err := config.Load("")
	if err != nil {
		return err
	}

	ws := file.Workspaces[name]
	// The real address changes at boot and is located at every connection
	// (session.connect).
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
	_, _ = fmt.Fprintf(out, "workspace %q -> %s@%s:%d\n",
		name, spec.Account, machinePlaceholderHost, spec.Port)

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
	key, err := keys.LoadOrCreateKey(config.KeyPath(), config.KeyComment())
	if err != nil {
		return "", err
	}
	return key.AuthorizedKey(config.KeyComment()), nil
}

func newMachineStartCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "start [name]",
		Short: "Start the machine",
		Long: `Starts the machine and returns once its agent is listening.

A background session serving this workspace is stopped first, because it holds
a connection to the machine's previous address.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withMachine(cmd, args, func(ctx context.Context, _ machine.Backend, name string, ws config.Workspace) error {
				// Any session here predates the boot and holds the old
				// address. Also catches one `stop` missed while it was still
				// binding its endpoint.
				stopSessionFor(cmd, name)

				// Held while waiting: an empty WSL machine shuts down again.
				hold, err := machine.Hold(ctx, ws.Machine.Backend, ws.Machine.Name)
				if err != nil {
					return err
				}
				defer func() { _ = hold.Close() }()

				// Located, so "started" means the agent is listening.
				if _, err := machine.Locate(ctx, ws.Machine.Backend, ws.Machine.Name, ws.Port); err != nil {
					return err
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "started %q\n", name)
				return nil
			})
		},
	}
}

func newMachineStopCommand() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "stop [name]",
		Short: "Stop the machine",
		Long: `Stops the background session serving this workspace, then the machine.

Its containers stop with it. Images, containers and volumes are kept. Refused
while the session is in use; -f overrides.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withMachine(cmd, args, func(ctx context.Context, b machine.Backend, name string, ws config.Workspace) error {
				if cfg, err := resolve([]string{name}); err == nil {
					if err := refuseInUse(cfg, force, "stopping the machine stops its containers", "machine stop"); err != nil {
						return err
					}
				}
				// The session first: it holds the machine open.
				stopSessionFor(cmd, name)

				if err := b.Stop(ctx, ws.Machine.Name); err != nil {
					return err
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "stopped %q\n", name)
				return nil
			})
		},
	}
	forceFlag(cmd, &force, "stop even if the workspace's session is in use")
	return cmd
}

func newMachineStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status [name]",
		Short: "Show whether the machine exists, runs, and matches its settings",
		Long:  `Exits 1 when the machine is not running.`,
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withMachine(cmd, args, func(ctx context.Context, b machine.Backend, _ string, ws config.Workspace) error {
				m := ws.Machine
				observed, err := b.Inspect(ctx, m.Name)
				if err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				row(out, "machine", fmt.Sprintf("%s (%s)", m.Name, m.Backend))
				row(out, "state", observed.State.String())
				reportGeneration(out, m, observed)
				if observed.State != machine.Running {
					return errNotReady
				}
				return nil
			})
		},
	}
}

// reportGeneration says whether the machine matches its recorded settings.
func reportGeneration(out io.Writer, m *config.Machine, observed machine.Observed) {
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

// withMachine looks up the machine behind the workspace args name (see
// machineName), and hands it to fn with that name.
func withMachine(cmd *cobra.Command, args []string, fn func(context.Context, machine.Backend, string, config.Workspace) error) error {
	name, err := machineName(args)
	if err != nil {
		return err
	}
	file, err := config.Load("")
	if err != nil {
		return err
	}
	ws, ok := file.Workspaces[name]
	if !ok {
		return noWorkspaceNamed(name)
	}
	if ws.Machine == nil {
		return fmt.Errorf("workspace %q is not backed by a machine this program built\n"+
			"  fix: these commands manage machines created with `%s`",
			name, ourCommand("machine create <name>"))
	}
	backend, err := findBackend(ws.Machine.Backend)
	if err != nil {
		return err
	}
	return fn(cmd.Context(), backend, name, ws)
}
