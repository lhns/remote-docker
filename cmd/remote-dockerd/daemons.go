package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/lhns/remote-docker/internal/server/daemons"
)

// The operator's handle on the per-account daemons.
//
// It exists because there was no way to act on them at all. A per-account
// daemon is created once and started forever after, so a setting changed in
// the stack file reached only accounts that did not yet have one -- and the
// remedy was a pair of `docker exec ... docker rm` commands somebody had to be
// told, with the container and volume names worked out by hand.
//
// The agent reconciles what it safely can on its own now (see
// daemons.Manager.reconcile). This is for the one case it cannot: a change of
// storage driver, where the graph cannot be migrated and the choice to discard
// it belongs to a person.
//
// Run inside the workspace container, where the parent daemon is:
//
//	docker exec <workspace> remote-dockerd daemons list
//	docker exec <workspace> remote-dockerd daemons reset alice
//	docker exec <workspace> remote-dockerd daemons reset --all --purge
func newDaemonsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemons",
		Short: "Inspect and reset the per-account Docker daemons",
	}
	cmd.AddCommand(newDaemonsListCommand(), newDaemonsResetCommand())
	return cmd
}

func newDaemonsListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List the accounts that have a daemon",
		RunE: func(cmd *cobra.Command, _ []string) error {
			m, err := managerForCommands()
			if err != nil {
				return err
			}
			accounts, err := m.Accounts(cmd.Context())
			if err != nil {
				return err
			}
			if len(accounts) == 0 {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "no per-account daemons")
				return nil
			}
			for _, a := range accounts {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%-16s %s\n", a, daemons.ContainerName(a))
			}
			return nil
		},
	}
}

func newDaemonsResetCommand() *cobra.Command {
	var all, purge bool

	cmd := &cobra.Command{
		Use:   "reset [account]",
		Short: "Remove an account's daemon so the next connection rebuilds it",
		Long: `Removes the daemon CONTAINER, which is disposable: the account's images and
containers live on a separate volume and are kept, so the daemon comes back
with whatever the workspace's current settings say.

With --purge that volume goes too. That is the account's entire Docker state,
and it is needed for exactly one thing: changing the storage driver, because a
graph written by one driver cannot be read by another.

Removing a daemon stops whatever it was running.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if (len(args) == 0) == !all {
				return fmt.Errorf("name an account, or pass --all")
			}

			m, err := managerForCommands()
			if err != nil {
				return err
			}

			accounts := args
			if all {
				if accounts, err = m.Accounts(cmd.Context()); err != nil {
					return err
				}
			}

			out := cmd.OutOrStdout()
			for _, account := range accounts {
				if err := m.Reset(cmd.Context(), account, purge); err != nil {
					return fmt.Errorf("resetting %s: %w", account, err)
				}
				what := "daemon"
				if purge {
					what = "daemon and storage"
				}
				_, _ = fmt.Fprintf(out, "removed %s's %s\n", account, what)
			}
			if len(accounts) == 0 {
				_, _ = fmt.Fprintln(out, "no per-account daemons")
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&all, "all", false, "every account with a daemon")
	cmd.Flags().BoolVar(&purge, "purge", false,
		"also delete the account's images and containers (needed only when the storage driver changes)")
	return cmd
}

// managerForCommands builds a Manager for the one-shot commands.
//
// Deliberately not the serving one: these run in a separate `docker exec`
// process that shares only the workspace's configuration. It reads the same
// environment, so `daemons ls` names the containers the running agent would.
func managerForCommands() (*daemons.Manager, error) {
	stateDir := envOr(envStateDir, "/etc/workspace")
	id, err := daemons.WorkspaceID(stateDir)
	if err != nil {
		return nil, err
	}
	return &daemons.Manager{
		Options: daemons.Options{Workspace: id},
		Log:     logger("daemons"),
	}, nil
}
