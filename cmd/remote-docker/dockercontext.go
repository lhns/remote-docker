package main

// Writing docker contexts, which is now something workspaces do rather than
// something you ask for.
//
// There used to be a `context` command alongside `workspace`, and between them
// no way to tell which you wanted: `workspace add` already created a context,
// and `context install` created it again. There is no case where somebody
// wants a workspace configured and not reachable as `docker --context <name>`,
// so it was never really a choice -- only a second place to look.

import (
	"fmt"
	"os/exec"
	"strings"

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
