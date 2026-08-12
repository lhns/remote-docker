package main

// Writing docker contexts, which is now something workspaces do rather than
// something you ask for.
//
// Contexts are written as a side effect of the `workspace` commands and are
// not exposed as commands of their own. Nobody wants a workspace configured
// and NOT reachable as `docker --context <name>`, so offering the two
// separately would only be a second place to look for the same thing.

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
// Written into every context this program creates, and read back to decide
// whether one is ours to replace or remove.
//
// It is what makes replacing a context safe. Contexts are named after the
// workspace so `docker --context dev ps` reads naturally, and a name like
// "dev" could easily already mean something else to the user. Destroying that
// would be a poor way to discover the collision.
//
// The string is a STORED VALUE, not a label: changing it orphans every context
// already written, which then cannot be updated or cleaned up and are silently
// treated as somebody else's. It stays what it has always been.
const contextMarker = "remote-docker workspace"

// dockerCmd runs a docker command on this machine's behalf.
//
// There is always one to run: a docker on PATH first, and when there is none
// this binary is one, so it invokes itself. Giving up instead left a machine
// with nothing installed, which is this project's premise, with no docker
// context at all, so every tool that resolves one found the platform default
// and reported that the daemon was not running.
//
// NoSessionEnv on both paths, because the docker on PATH may be this binary
// under its other name. Without it, writing a context would spawn us and we
// would open an SSH connection, an NFS server and a reverse tunnel in order to
// write a file on this machine. A real docker CLI ignores the variable.
func dockerCmd(args ...string) *exec.Cmd {
	name, argv := dockerInvocation(exec.LookPath, os.Executable, args)
	cmd := exec.Command(name, argv...)
	cmd.Env = append(os.Environ(), NoSessionEnv+"=1")
	return cmd
}

// dockerInvocation decides what to run and with which arguments.
//
// Separated from the exec so the fallback can be tested without one.
//
// The arguments are the same either way: this binary's root IS the Docker CLI
// (ADR 0024), so `context inspect x` means the same thing to us as to a docker
// on PATH. One function answers "how do I invoke docker" so that no caller has
// to remember whether its arguments need shifting.
func dockerInvocation(
	lookPath func(string) (string, error),
	executable func() (string, error),
	args []string,
) (string, []string) {
	if path, err := lookPath("docker"); err == nil {
		return path, args
	}
	self, err := executable()
	if err != nil {
		// Nothing better to try. The command will fail and say so, which is
		// more useful than deciding here that there is no docker.
		return "docker", args
	}
	return self, args
}

type installedContext struct {
	name     string
	endpoint string
}

// installContext writes one workspace's context, refusing to replace a context
// this client did not create.
func installContext(cfg config.Config) (installedContext, error) {
	name := cfg.ContextName()
	endpoint := dockerHostOf(cfg)

	if contextIsOurs(name) {
		// Ours, so replacing is safe, and it is replaced rather than
		// updated, so a stale endpoint from an earlier run cannot survive.
		_ = dockerCmd("context", "rm", "-f", name).Run()
	} else if contextExists(name) {
		return installedContext{}, fmt.Errorf(
			"a docker context named %q already exists and was not created by remote-docker, "+
				"so it will not be replaced; rename the workspace, or remove that context yourself",
			name)
	}

	create := dockerCmd("context", "create", name,
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
func contextIsOurs(name string) bool {
	out, err := dockerCmd("context", "inspect", name).Output()
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

func contextExists(name string) bool {
	return dockerCmd("context", "inspect", name).Run() == nil
}
