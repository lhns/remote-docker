package main

// Which workspace, and where its Docker API is served locally.
//
// The endpoint is derived from the workspace rather than stored beside it, so
// there is one spelling and no pair to disagree.

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

// endpointOf is where this workspace's Docker API is served locally.
//
// One spelling, in one place, and that is the point rather than the brevity.
// `endpointOf(cfg)` was written out at ten call
// sites, and the argument is the part that matters: passing an empty base
// instead used to derive the RELATIVE path "-dev" for a named workspace, which
// meant a socket in whatever directory the process happened to be in and a
// docker context reading unix://-dev. That could not be reproduced on Windows,
// where the pipe name is a real constant, and CI never saw it because the
// suites set an endpoint explicitly.
func endpointOf(cfg config.Config) string {
	return cfg.EndpointFor(proxy.DefaultEndpoint())
}

// dockerHostOf is the same endpoint as a DOCKER_HOST value.
func dockerHostOf(cfg config.Config) string {
	return proxy.DockerHost(endpointOf(cfg))
}
