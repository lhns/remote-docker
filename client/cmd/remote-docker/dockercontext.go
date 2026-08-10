package main

// Writing docker contexts, which is now something workspaces do rather than
// something you ask for.
//
// There used to be a `context` command alongside `workspace`, and between them
// no way to tell which you wanted: `workspace add` already created a context,
// and `context install` created it again. There is no case where somebody
// wants a workspace configured and not reachable as `docker --context <name>`,
// so it was never really a choice, only a second place to look.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/lhns/remote-docker/client/internal/config"
)

// contextMarker identifies a docker context as one this client wrote.
//
// It is what makes replacing a context safe. Contexts are named after the
// workspace so `docker --context dev ps` reads naturally, and a name like
// "dev" could easily already mean something else to the user. Destroying that
// would be a poor way to discover the collision, so a context is only replaced
// when its description says we wrote it.
const contextMarker = "remote-docker workspace"

// dockerCmd runs a docker command on this machine's behalf.
//
// Every docker invocation in this file goes through it, for one reason: once
// `shim install` has put a `docker` on PATH, the docker that LookPath finds is
// THIS BINARY. Without NoSessionEnv, writing a context would spawn us, and we
// would open an SSH connection, an NFS server and a reverse tunnel in order to
// write a file on this machine, and then tear them all down again.
//
// It costs nothing when the docker found is a real one: a docker CLI that has
// never heard of the variable ignores it.
func dockerCmd(docker string, args ...string) *exec.Cmd {
	cmd := exec.Command(docker, args...)
	cmd.Env = append(os.Environ(), NoSessionEnv+"=1")
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
	endpoint := dockerHostOf(cfg)

	if contextIsOurs(docker, name) {
		// Ours, so replacing is safe, and it is replaced rather than
		// updated, so a stale endpoint from an earlier run cannot survive.
		_ = dockerCmd(docker, "context", "rm", "-f", name).Run()
	} else if contextExists(docker, name) {
		return installedContext{}, fmt.Errorf(
			"a docker context named %q already exists and was not created by remote-docker, "+
				"so it will not be replaced; rename the workspace, or remove that context yourself",
			name)
	}

	create := dockerCmd(docker, "context", "create", name,
		"--description", contextMarker,
		"--docker", "host="+endpoint)
	if out, err := create.CombinedOutput(); err != nil {
		return installedContext{}, fmt.Errorf("creating the docker context: %w: %s", err, out)
	}
	return installedContext{name: name, endpoint: endpoint}, nil
}

// contextIsOurs reports whether a docker context carries our marker.
//
// The JSON is parsed rather than asked for with `--format`, and that is not a
// preference. A template like `{{.Metadata.Description}}` matches the JSON
// docker prints, but it runs against docker's internal store.Metadata, where
// Description sits one level deeper, so it fails to evaluate for every context
// that has ever existed.
//
// Output() discards stderr, so that failure arrives as an error and every
// context is then judged not ours: `workspace rm` leaves the context behind in
// silence, and `workspace create` refuses to replace a context it wrote
// itself. The JSON shape is documented and stable; the path through docker's
// own structs is neither.
func contextIsOurs(docker, name string) bool {
	out, err := dockerCmd(docker, "context", "inspect", name).Output()
	if err != nil {
		return false
	}
	var contexts []struct {
		Metadata struct {
			Description string `json:"Description"`
		} `json:"Metadata"`
	}
	if err := json.Unmarshal(out, &contexts); err != nil || len(contexts) == 0 {
		return false
	}
	return strings.TrimSpace(contexts[0].Metadata.Description) == contextMarker
}

func contextExists(docker, name string) bool {
	return dockerCmd(docker, "context", "inspect", name).Run() == nil
}
