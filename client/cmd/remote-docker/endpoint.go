package main

// Which workspace, and where its Docker API is served locally. The endpoint is
// derived from the workspace, never stored.

import (
	"fmt"
	"os"

	"github.com/lhns/remote-docker/client/internal/config"
	"github.com/lhns/remote-docker/client/internal/proxy"
)

func resolve() (config.Config, error) {
	return config.Resolve(overrides, "")
}

// requireHost is config's RequireHost with the remedy spelled for this binary.
func requireHost(cfg config.Config) error {
	if err := cfg.RequireHost(); err != nil {
		return fmt.Errorf("%w\n  fix: `%s`, or set %s",
			err, ourCommand("create <name> --host <host>"), config.EnvHost)
	}
	return nil
}

// noWorkspaceNamed is the one answer to a name that is not in the config.
func noWorkspaceNamed(name string) error {
	return fmt.Errorf("no workspace named %q\n  fix: `%s` lists them", name, ourCommand("ls"))
}

// exportLine renders the DOCKER_HOST assignment for the shell the user is
// most likely holding.
func exportLine(endpoint string) string {
	if os.PathSeparator == '\\' {
		return fmt.Sprintf("$env:DOCKER_HOST = %q", endpoint)
	}
	return fmt.Sprintf("export DOCKER_HOST=%s", endpoint)
}

// endpointOf is where this workspace's Docker API is served locally. Never pass
// EndpointFor an empty base (see config.EndpointFor); no test would notice.
func endpointOf(cfg config.Config) string {
	return cfg.EndpointFor(proxy.DefaultEndpoint())
}

// dockerHostOf is the same endpoint as a DOCKER_HOST value.
func dockerHostOf(cfg config.Config) string {
	return proxy.DockerHost(endpointOf(cfg))
}
