package main

// `remote host-ports`: the ports containers reach on this machine (ADR 0057).
// The same field as the host-ports setting and REMOTE_DOCKER_HOST_PORTS, so
// either way of setting it keeps working; the environment variable overrides
// the file here as it does for every setting.

import (
	"cmp"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/lhns/remote-docker/client/internal/config"
	"github.com/lhns/remote-docker/client/internal/proxy"
)

func newHostPortsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "host-ports",
		Short: "List, add and remove the local ports containers can reach",
		Long: `Containers you create reach these TCP ports on this machine as
host.docker.internal:<port>. Nothing is reachable unless listed, and every
container of your account can then reach a listed port, so do not list a
service that trusts whoever connects from localhost.

The list is the workspace's host-ports setting, so REMOTE_DOCKER_HOST_PORTS and
"remote set --host-ports" work too; the variable overrides the file. A running
session applies a change at once, with no restart.`,
		Args: onlySubcommands,
		RunE: helpWhenBare,
	}
	cmd.AddCommand(
		newHostPortsListCommand(),
		newHostPortsChangeCommand("add", "Add ports to the list", func(have, given []int) []int {
			return append(slices.Clone(have), given...)
		}),
		newHostPortsChangeCommand("rm", "Remove ports from the list", func(have, given []int) []int {
			return slices.DeleteFunc(slices.Clone(have), func(p int) bool { return slices.Contains(given, p) })
		}),
		newHostPortsChangeCommand("set", "Replace the list", func(_, given []int) []int { return given }),
	)
	return cmd
}

func newHostPortsListCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List the ports and whether each is forwarded",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := resolve(nil)
			if err != nil {
				return err
			}
			ports, err := config.ParseHostPorts(cfg.HostPorts)
			if err != nil {
				return err
			}
			printHostPorts(cmd, cfg, ports)
			return nil
		},
	}
}

// newHostPortsChangeCommand is add, rm and set: compute the new list from the
// file's, save it, and tell a running session.
func newHostPortsChangeCommand(use, short string, change func(have, given []int) []int) *cobra.Command {
	return &cobra.Command{
		Use:   use + " <port>...",
		Short: short,
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			given, err := config.ParseHostPorts(strings.Join(args, ","))
			if err != nil {
				return err
			}
			cfg, err := resolve(nil)
			if err != nil {
				return err
			}
			file, err := config.Load("")
			if err != nil {
				return err
			}
			ws := &file.Workspace
			if cfg.Name != "" {
				entry, ok := file.Workspaces[cfg.Name]
				if !ok {
					return noWorkspaceNamed(cfg.Name)
				}
				ws = &entry
			}
			// What the file says, not the environment: the file is what is edited.
			fileList, err := config.ParseHostPorts(cmp.Or(ws.HostPorts, file.HostPorts))
			if err != nil {
				return err
			}
			next := change(fileList, given)
			slices.Sort(next)
			next = slices.Compact(next)

			ws.HostPorts = config.HostPortList(next)
			if cfg.Name != "" {
				file.Workspaces[cfg.Name] = *ws
			}
			if err := saveConfig(file, ""); err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if config.SetInEnvironment("host-ports") {
				_, _ = fmt.Fprintf(out, "saved, but %s is set and overrides it here and in any session started from this environment\n",
					config.EnvHostPorts)
				cfg.HostPorts = ws.HostPorts
				printHostPorts(cmd, cfg, next)
				return nil
			}
			cfg.HostPorts = ws.HostPorts
			if err := applyHostPorts(cfg, next); err != nil {
				_, _ = fmt.Fprintf(out, "saved, but the running session could not take it: %s\n  fix: `%s`\n",
					firstLine(err.Error()), ourCommand("restart"))
			}
			printHostPorts(cmd, cfg, next)
			return nil
		},
	}
}

// applyHostPorts tells the running background session, if there is one. No
// session is not an error: the next one reads the file.
func applyHostPorts(cfg config.Config, ports []int) error {
	endpoint := endpointOf(cfg)
	if !proxy.Reachable(endpoint) {
		return nil
	}
	path := "host-ports?ports=" + url.QueryEscape(config.HostPortList(ports))
	return controlWithin(30*time.Second, endpoint, http.MethodPost, path, nil)
}

// printHostPorts is the list and, for a running session, what became of each.
func printHostPorts(cmd *cobra.Command, cfg config.Config, ports []int) {
	out := cmd.OutOrStdout()
	if config.SetInEnvironment("host-ports") {
		_, _ = fmt.Fprintf(out, "%s is set, so it is the list and overrides the file\n", config.EnvHostPorts)
	}
	if len(ports) == 0 {
		_, _ = fmt.Fprintln(out, "no host ports listed")
		return
	}

	var local proxy.Status
	answering := false
	if endpoint := endpointOf(cfg); proxy.Reachable(endpoint) {
		answering = control(endpoint, http.MethodGet, "status", &local) == nil
	}

	for _, port := range ports {
		state := "listed; no session is running, so it is forwarded when one starts"
		switch {
		case answering && !local.Connected:
			state = "listed; forwarded when the session next connects"
		case answering:
			state = "listed, but the running session has not applied it\n  fix: `" + ourCommand("restart") + "`"
			prefix := fmt.Sprintf("%d: ", port)
			for _, line := range local.HostForwards {
				if rest, ok := strings.CutPrefix(line, prefix); ok {
					state = rest
				}
			}
		}
		_, _ = fmt.Fprintf(out, "%d  %s\n", port, state)
	}
}
