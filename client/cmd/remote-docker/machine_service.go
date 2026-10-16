package main

// The Hyper-V service commands (ADR 0055): `remote machine service`, hidden,
// run by the service manager and the installer and never by hand. And the one
// decision the user-facing commands make about it.

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/lhns/remote-docker/client/internal/config"
	"github.com/lhns/remote-docker/machine"
)

// brokerInstalled is machine.BrokerInstalled, replaced in tests.
var brokerInstalled = machine.BrokerInstalled

// decideBroker says whether a Hyper-V machine is made and run through the
// service. A machine that exists keeps what it was built with: one made by
// PowerShell in this session has no record at the service, and one the service
// made cannot be reached any other way. A new one uses the service whenever it
// is installed, which is what lets a user who is not an administrator have one.
func decideBroker(spec *machine.Spec, recorded *config.Workspace, rootfsFlag string) error {
	spec.Broker = false
	if spec.Backend != "hyperv" {
		return nil
	}
	if recorded != nil && recorded.Machine != nil {
		spec.Broker = recorded.Machine.Broker
	} else if ok, err := brokerInstalled(); err == nil {
		spec.Broker = ok
	}
	if spec.Broker && rootfsFlag != "" {
		return errors.New("--rootfs cannot be used with the Hyper-V service, which downloads its own Flatcar disk\n" +
			"  fix: omit --rootfs")
	}
	return nil
}

// refuseExistingWorkspace fails when a workspace entry or a Hyper-V machine
// already has this name. The installer runs it just before its rollback
// `remote rm`, which a failure here keeps out of the rollback script, so that
// rm only ever removes what the install's own create made (ADR 0048).
func refuseExistingWorkspace(ctx context.Context, name string) error {
	file, err := config.Load("")
	if err != nil {
		return err
	}
	if _, ok := file.Workspaces[name]; ok {
		return fmt.Errorf("a workspace named %q already exists, and setup will not replace it\n"+
			"  fix: choose another workspace name, or remove that one first with `%s`", name, ourCommand("rm "+name))
	}
	spec := machine.Spec{Name: name, Backend: "hyperv"}
	if err := decideBroker(&spec, nil, ""); err != nil {
		return err
	}
	backend, err := findBackend(spec.BackendKey())
	if err != nil {
		return err
	}
	observed, err := backend.Inspect(ctx, name)
	if err != nil {
		return fmt.Errorf("cannot tell whether a machine named %q exists: %w", name, err)
	}
	if observed.State != machine.Absent {
		return fmt.Errorf("a Hyper-V machine named %q already exists with no workspace for it\n"+
			"  fix: choose another workspace name", name)
	}
	return nil
}

func newMachineServiceCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "service",
		Short:  "The Hyper-V service (run by the installer)",
		Hidden: true,
		Args:   onlySubcommands,
		RunE:   helpWhenBare,
	}

	var userSID string
	setup := &cobra.Command{
		Use:   "setup",
		Short: "Create the " + machine.BrokerGroup + " group and add a user to it",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			if userSID == "" {
				return errors.New("--user-sid is required")
			}
			return machine.BrokerSetup(userSID)
		},
	}
	setup.Flags().StringVar(&userSID, "user-sid", "", "the SID of the user who may use the service")

	cmd.AddCommand(
		&cobra.Command{
			Use:   "run",
			Short: "Run the service (started by the service manager)",
			Args:  cobra.NoArgs,
			RunE:  func(*cobra.Command, []string) error { return machine.RunBrokerService() },
		},
		setup,
		&cobra.Command{
			Use:   "refuse-existing <name>",
			Short: "Fail if a workspace or machine already has this name",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return refuseExistingWorkspace(cmd.Context(), args[0])
			},
		},
		&cobra.Command{
			Use:   "teardown",
			Short: "Destroy the service's machines and undo setup",
			Args:  cobra.NoArgs,
			RunE:  func(cmd *cobra.Command, _ []string) error { return machine.BrokerTeardown(cmd.Context()) },
		},
		&cobra.Command{
			Use:   "purge",
			Short: "Destroy every machine the service owns",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				names, err := machine.BrokerPurgeLocal(cmd.Context())
				for _, n := range names {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "destroyed %q\n", n)
				}
				return err
			},
		},
	)
	return cmd
}
