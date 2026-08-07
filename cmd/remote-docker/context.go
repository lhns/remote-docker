package main

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"github.com/lhns/remote-docker/internal/client/config"
	"github.com/lhns/remote-docker/internal/client/proxy"
)

// contextMarker identifies a docker context as one this client wrote.
//
// It is what makes replacing a context safe. Contexts are named after the
// workspace so `docker --context dev ps` reads naturally, and a name like
// "dev" could easily already mean something else to the user. Destroying that
// would be a poor way to discover the collision, so a context is only replaced
// when its description says we wrote it.
const contextMarker = "remote-docker workspace"

// newContextCommand manages Docker contexts pointing at workspaces.
//
// This is how the official docker and compose CLIs reach a workspace with no
// environment variables and no administrator rights. A context is per-user
// configuration under ~/.docker, so installing one needs no privileges, and it
// works identically on Windows, Linux and macOS -- unlike binding the daemon's
// well-known socket, which on Linux would mean writing /var/run/docker.sock
// and therefore root.
func newContextCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "context",
		Short: "Manage the docker contexts pointing at your workspaces",
	}
	cmd.AddCommand(newContextInstallCommand(), newContextRemoveCommand())
	return cmd
}

func newContextInstallCommand() *cobra.Command {
	var use, all bool

	cmd := &cobra.Command{
		Use:   "install",
		Short: "Create docker contexts for your workspaces",
		Long: `Creates a docker context per workspace, pointing at that workspace's
endpoint, so the official docker and compose CLIs reach it with no environment
variables set.

Each workspace gets its own context and its own endpoint, so several can be
open at once:

    docker --context dev ps
    docker --context ci ps

Needs no administrator rights: a context is per-user configuration under
~/.docker.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			docker, err := exec.LookPath("docker")
			if err != nil {
				return fmt.Errorf("no docker CLI found on PATH, so there is no context store to write to; " +
					"use the built-in client instead -- `remote-docker docker ...` needs no context")
			}

			names, err := targetWorkspaces(all)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			var installed []installedContext
			for _, name := range names {
				cfg, err := config.Resolve(config.Overrides{Workspace: name}, "")
				if err != nil {
					return err
				}
				written, err := installContext(docker, cfg)
				if err != nil {
					return err
				}
				installed = append(installed, written)
				_, _ = fmt.Fprintf(out, "context %-14s -> %s\n", written.name, written.endpoint)
			}

			// --use only makes sense for one context; selecting the last of
			// several would be an arbitrary choice presented as a decision.
			switch {
			case use && len(installed) == 1:
				if o, err := exec.Command(docker, "context", "use", installed[0].name).CombinedOutput(); err != nil {
					return fmt.Errorf("selecting the docker context: %w: %s", err, o)
				}
				_, _ = fmt.Fprintln(out, "selected it as the active context")
			case use:
				_, _ = fmt.Fprintf(out, "\n--use applies to a single workspace; select one with:\n")
				fallthrough
			default:
				_, _ = fmt.Fprintf(out, "\n    docker context use %s\n", installed[0].name)
				_, _ = fmt.Fprintf(out, "or per command:\n    docker --context %s ps\n", installed[0].name)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&use, "use", false, "also make it the active context (single workspace only)")
	cmd.Flags().BoolVar(&all, "all", false, "install a context for every configured workspace")
	return cmd
}

type installedContext struct {
	name     string
	endpoint string
}

// installContext writes one workspace's context, refusing to replace a context
// this client did not create.
func installContext(docker string, cfg config.Config) (installedContext, error) {
	name := cfg.ContextName()
	endpoint := proxy.DockerHost(cfg.EndpointFor(proxy.DefaultEndpoint))

	if contextIsOurs(docker, name) {
		// Ours, so replacing is safe -- and it is replaced rather than
		// updated, so a stale endpoint from an earlier run cannot survive.
		_ = exec.Command(docker, "context", "rm", "-f", name).Run()
	} else if contextExists(docker, name) {
		return installedContext{}, fmt.Errorf(
			"a docker context named %q already exists and was not created by remote-docker, "+
				"so it will not be replaced; rename the workspace, or remove that context yourself",
			name)
	}

	create := exec.Command(docker, "context", "create", name,
		"--description", contextMarker,
		"--docker", "host="+endpoint)
	if out, err := create.CombinedOutput(); err != nil {
		return installedContext{}, fmt.Errorf("creating the docker context: %w: %s", err, out)
	}
	return installedContext{name: name, endpoint: endpoint}, nil
}

// contextIsOurs reports whether a context exists and carries our marker.
func contextIsOurs(docker, name string) bool {
	out, err := exec.Command(docker, "context", "inspect", name,
		"--format", "{{.Metadata.Description}}").Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == contextMarker
}

func contextExists(docker, name string) bool {
	return exec.Command(docker, "context", "inspect", name).Run() == nil
}

// targetWorkspaces resolves which workspaces a context command applies to.
// The empty name means "whichever one the ordinary resolution picks".
func targetWorkspaces(all bool) ([]string, error) {
	if !all {
		return []string{""}, nil
	}
	file, err := config.Load("")
	if err != nil {
		return nil, err
	}
	names := file.Names()
	if len(names) == 0 {
		return nil, fmt.Errorf("--all was given but no named workspaces are configured")
	}
	return names, nil
}

func newContextRemoveCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "remove",
		Short: "Remove the docker context for a workspace",
		RunE: func(cmd *cobra.Command, _ []string) error {
			docker, err := exec.LookPath("docker")
			if err != nil {
				return fmt.Errorf("no docker CLI found on PATH")
			}
			cfg, err := resolve()
			if err != nil {
				return err
			}
			name := cfg.ContextName()

			if !contextIsOurs(docker, name) {
				return fmt.Errorf("docker context %q was not created by remote-docker; leaving it alone", name)
			}

			// Select default first: removing the active context would leave
			// the CLI pointing at one that no longer exists.
			_ = exec.Command(docker, "context", "use", "default").Run()

			if out, err := exec.Command(docker, "context", "rm", "-f", name).CombinedOutput(); err != nil {
				return fmt.Errorf("removing the docker context: %w: %s", err, out)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "removed docker context %q\n", name)
			return nil
		},
	}
}
