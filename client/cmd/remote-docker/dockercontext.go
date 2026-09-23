package main

// Docker contexts, written as a side effect of the `workspace` commands.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/lhns/remote-docker/client/internal/config"
)

// contextMarker is the description of every context we write, and the only
// thing that lets us replace or remove one. A STORED value: changing it
// silently turns every existing context into somebody else's.
const contextMarker = "remote-docker workspace"

// dockerCmd runs a docker on PATH, or else this binary, which is one. With
// NoSessionEnv, since the docker on PATH may be us too.
func dockerCmd(args ...string) *exec.Cmd {
	cmd := exec.Command(dockerProgram(exec.LookPath, os.Executable), args...)
	cmd.Env = append(os.Environ(), NoSessionEnv+"=1")
	return cmd
}

// dockerProgram decides which docker to run.
func dockerProgram(lookPath func(string) (string, error), executable func() (string, error)) string {
	if path, err := lookPath("docker"); err == nil {
		return path
	}
	self, err := executable()
	if err != nil {
		// Let the command fail and say so.
		return "docker"
	}
	return self
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
		// Replaced rather than updated, so no stale endpoint survives.
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
// JSON, not `--format '{{.Metadata.Description}}'`: the template runs against
// docker's internal struct, where Description is nested deeper, so it fails for
// every context and each is silently judged not ours.
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
