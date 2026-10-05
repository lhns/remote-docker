package main

import (
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/lhns/remote-docker/agent/internal/ephemeral"
)

// The operator's view of the ephemeral runs (ADR 0050), read from the record
// the serving agent keeps:
//
//	docker exec <workspace> remote-dockerd ephemeral ls
func newEphemeralCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ephemeral",
		Short: "Inspect the runs of ephemeral accounts",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(&cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List the runs holding a port, per account, with their state and age",
		Long: `Lists each run that holds a reverse-tunnel port, as the serving agent last
recorded it. AGE is the time since a connection of the run last started or
ended. A run whose grace has run out shows grace until the next sweep.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			runs, err := ephemeral.List(envOr(envStateDir, defaultStateDir))
			if err != nil {
				return err
			}
			printRuns(cmd.OutOrStdout(), runs, time.Now())
			return nil
		},
	})
	return cmd
}

// printRuns writes one line per run, by account and then client.
func printRuns(out io.Writer, runs []ephemeral.Entry, now time.Time) {
	if len(runs) == 0 {
		_, _ = fmt.Fprintln(out, "no ephemeral runs")
		return
	}
	sort.Slice(runs, func(i, j int) bool {
		if runs[i].Account != runs[j].Account {
			return runs[i].Account < runs[j].Account
		}
		return runs[i].Client < runs[j].Client
	})
	_, _ = fmt.Fprintf(out, "%-16s %-8s %-8s %-5s %s\n", "ACCOUNT", "CLIENT", "STATE", "PORT", "AGE")
	for _, r := range runs {
		_, _ = fmt.Fprintf(out, "%-16s %-8s %-8s %-5d %s\n",
			r.Account, r.Client, r.State, r.Port, now.Sub(r.LastSeen).Round(time.Second))
	}
}
