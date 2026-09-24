package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/lhns/remote-docker/agent/internal/daemons"
)

// The operator's handle on the per-account daemons.
//
// A per-account daemon is created once and started forever after, so a setting
// changed in the stack file reaches only accounts that do not yet have one. The
// agent reconciles what it safely can by itself (see daemons.Manager.reconcile).
// This is for the one case it cannot: a change of storage driver, where the
// graph cannot be migrated, so discarding somebody's images is a decision that
// belongs to a person.
//
// Run inside the workspace container, where the parent daemon is:
//
//	docker exec <workspace> remote-dockerd daemons ls
//	docker exec <workspace> remote-dockerd daemons reset alice
//	docker exec <workspace> remote-dockerd daemons reset --all --purge
func newDaemonsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemons",
		Short: "Inspect and reset the per-account Docker daemons",
		// A word that is not a subcommand is an error rather than a help
		// screen with exit 0. cobra shows help before checking Args unless
		// the command can run, hence the RunE.
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newDaemonsListCommand(), newDaemonsResetCommand())
	return cmd
}

func newDaemonsListCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List the accounts that have a daemon",
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
	var all, purge, force bool

	cmd := &cobra.Command{
		Use:   "reset [account]",
		Short: "Remove an account's daemon so the next connection rebuilds it",
		Long: `Removes the daemon CONTAINER, which is disposable: the account's images and
containers live on a separate volume and are kept, so the daemon comes back
with whatever the workspace's current settings say.

With --purge that volume goes too. That is the account's entire Docker state,
and it is needed for exactly one thing: changing the storage driver, because a
graph written by one driver cannot be read by another.

Removing a daemon stops whatever it was running, so it is refused while the
daemon runs containers; -f overrides.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if (len(args) == 0) == !all {
				return fmt.Errorf("name one account, or pass --all\n" +
					"  fix: `remote-dockerd daemons ls` lists them")
			}

			m, err := daemonsManager()
			if err != nil {
				return err
			}

			accounts := args
			if all {
				if accounts, err = m.Accounts(cmd.Context()); err != nil {
					return err
				}
			}
			return resetDaemons(cmd, m, accounts, purge, force)
		},
	}

	cmd.Flags().BoolVar(&all, "all", false, "every account with a daemon")
	cmd.Flags().BoolVar(&purge, "purge", false,
		"also delete the account's images and containers (needed only when the storage driver changes)")
	cmd.Flags().BoolVarP(&force, "force", "f", false, "reset even while the daemon runs containers")
	return cmd
}

// daemonResetter is what reset asks of a daemons.Manager.
type daemonResetter interface {
	Accounts(ctx context.Context) ([]string, error)
	Exists(ctx context.Context, account string) bool
	Running(ctx context.Context, account string) int
	Reset(ctx context.Context, account string, purge bool) error
}

// daemonsManager is managerForCommands, replaced in tests.
var daemonsManager = func() (daemonResetter, error) { return managerForCommands() }

// resetDaemons resets each account's daemon, refusing all of them unless
// forced when any is running containers, before anything is removed.
func resetDaemons(cmd *cobra.Command, m daemonResetter, accounts []string, purge, force bool) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	if len(accounts) == 0 {
		_, _ = fmt.Fprintln(out, "no per-account daemons")
		return nil
	}

	if !force {
		for _, account := range accounts {
			fix := fmt.Sprintf("  fix: `remote-dockerd daemons reset %s -f` to stop them anyway", account)
			switch n := m.Running(ctx, account); {
			case n < 0:
				return fmt.Errorf("cannot tell whether %s's daemon is running containers\n%s", account, fix)
			case n > 0:
				return fmt.Errorf("%s's daemon is running %d container(s), and resetting it stops them\n%s", account, n, fix)
			}
		}
	}

	what := "daemon"
	if purge {
		what = "daemon and storage"
	}
	for _, account := range accounts {
		if !m.Exists(ctx, account) {
			_, _ = fmt.Fprintf(out, "no daemon for %s\n", account)
			continue
		}
		if err := m.Reset(ctx, account, purge); err != nil {
			return fmt.Errorf("resetting %s: %w", account, err)
		}
		_, _ = fmt.Fprintf(out, "removed %s's %s\n", account, what)
	}
	return nil
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
