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
