package main

import (
	"fmt"
	"os/exec"

	"github.com/spf13/cobra"

	"github.com/lhns/remote-docker/internal/client/proxy"
)

// contextName is the docker context this client installs.
const contextName = "remote-docker"

// newContextCommand manages a Docker context pointing at our endpoint.
//
// This is how the official docker and compose CLIs reach the workspace with no
// environment variables and no administrator rights. A context is per-user
// configuration under ~/.docker, so installing one needs no privileges, and it
// works identically on Windows, Linux and macOS -- unlike binding the daemon's
// well-known socket, which on Linux would mean writing /var/run/docker.sock
// and therefore root.
func newContextCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "context",
		Short: "Manage the docker context pointing at this workspace",
	}
	cmd.AddCommand(newContextInstallCommand(), newContextRemoveCommand())
	return cmd
}

func newContextInstallCommand() *cobra.Command {
	var use bool

	cmd := &cobra.Command{
		Use:   "install",
		Short: "Create a docker context for this workspace",
		Long: `Creates a docker context named "remote-docker" pointing at this client's
endpoint, so the official docker and compose CLIs reach the workspace with no
environment variables set.

Needs no administrator rights: a context is per-user configuration under
~/.docker.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			endpoint := proxy.DockerHost(cfg.Endpoint)

			docker, err := exec.LookPath("docker")
			if err != nil {
				return fmt.Errorf(
					"no docker CLI found on PATH, so there is no context store to write to.\n"+
						"Use the built-in client instead -- `remote-docker docker ...` needs no context --\n"+
						"or set DOCKER_HOST=%s", endpoint)
			}

			// Recreated rather than updated, so a stale endpoint from an
			// earlier run cannot survive.
			_ = exec.Command(docker, "context", "rm", "-f", contextName).Run()

			create := exec.Command(docker, "context", "create", contextName,
				"--description", "remote-docker workspace",
				"--docker", "host="+endpoint)
			if out, err := create.CombinedOutput(); err != nil {
				return fmt.Errorf("creating the docker context: %w: %s", err, out)
			}

			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "created docker context %q -> %s\n", contextName, endpoint)

			if use {
				if out, err := exec.Command(docker, "context", "use", contextName).CombinedOutput(); err != nil {
					return fmt.Errorf("selecting the docker context: %w: %s", err, out)
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "selected it as the active context\n")
			} else {
				_, _ = fmt.Fprintf(out, "\nUse it with:\n    docker context use %s\n", contextName)
				_, _ = fmt.Fprintf(out, "or per command:\n    docker --context %s ps\n", contextName)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&use, "use", false, "also make it the active context")
	return cmd
}

func newContextRemoveCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "remove",
		Short: "Remove the docker context for this workspace",
		RunE: func(cmd *cobra.Command, _ []string) error {
			docker, err := exec.LookPath("docker")
			if err != nil {
				return fmt.Errorf("no docker CLI found on PATH")
			}
			// Selecting default first, or removing the active context leaves
			// the CLI pointing at one that no longer exists.
			_ = exec.Command(docker, "context", "use", "default").Run()

			if out, err := exec.Command(docker, "context", "rm", "-f", contextName).CombinedOutput(); err != nil {
				return fmt.Errorf("removing the docker context: %w: %s", err, out)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "removed docker context %q\n", contextName)
			return nil
		},
	}
}
