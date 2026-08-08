// Package elevate lets the workspace run privileged under Docker Swarm
// without a separate launcher image.
//
// Swarm cannot run privileged tasks. The usual answer is a third-party
// launcher container that starts the real one through the host's Docker
// socket. This does the same thing with no extra image: the service starts
// unprivileged, inspects itself, and relaunches itself privileged outside
// Swarm. See docs/adr/0013.
package elevate

import (
	"fmt"
	"strings"
)

// ElevatedEnv marks a container as already elevated.
//
// Without it a misconfiguration -- the child somehow starting with the elevate
// command again -- would fork containers until the node fell over. The guard
// costs one environment variable.
const ElevatedEnv = "WORKSPACE_ELEVATED"

// NameSuffix is appended to our own container name to name the child.
const NameSuffix = ".elevated"

// Mount is one mount on a container.
type Mount struct {
	Type string

	// Name is the volume name when Type is "volume". Replicating a volume
	// mount by its host path instead of its name would bypass the volume
	// entirely and bind the daemon's internal storage directory, which
	// happens to work and is quietly wrong.
	Name string

	Source      string
	Destination string
	ReadOnly    bool
}

// arg renders the mount as a -v value.
func (m Mount) arg() string {
	source := m.Source
	if m.Type == "volume" && m.Name != "" {
		source = m.Name
	}
	spec := source + ":" + m.Destination
	if m.ReadOnly {
		spec += ":ro"
	}
	return spec
}

// ContainerInfo is what we learn about ourselves from the host daemon.
type ContainerInfo struct {
	ID     string
	Name   string
	Image  string
	Mounts []Mount
	Env    []string
}

// RunSpec describes the privileged container to launch.
type RunSpec struct {
	Name       string
	Image      string
	Network    string
	Privileged bool
	Remove     bool
	Mounts     []Mount
	Env        []string
}

// Args renders the spec as arguments to `docker run`.
//
// Kept next to Plan and pure for the same reason: the difference between a
// correct and a catastrophic invocation here is one flag, and it should be
// visible in a test rather than buried in a command line.
func (s RunSpec) Args() []string {
	args := []string{"run", "-i"}

	// Read from the field, which it did not used to be: `--rm` was appended
	// unconditionally and `Remove` was set but never consulted. It happened to
	// be harmless because the only caller wants --rm -- elevate's child is a
	// singleton whose state is worthless -- but a spec with a flag it ignores
	// is a trap for the next caller, and the next caller is a per-user daemon
	// holding somebody's running work.
	if s.Remove {
		args = append(args, "--rm")
	}
	if s.Privileged {
		args = append(args, "--privileged")
	}
	if s.Network != "" {
		args = append(args, "--network", s.Network)
	}
	if s.Name != "" {
		args = append(args, "--name", s.Name)
	}
	for _, e := range s.Env {
		args = append(args, "-e", e)
	}
	for _, m := range s.Mounts {
		args = append(args, "-v", m.arg())
	}
	return append(args, s.Image)
}

// Options tune the plan.
type Options struct {
	// HostSocket is where the host's Docker socket is mounted in this
	// container. Any mount of it is dropped from the child.
	HostSocket string
}

// DefaultHostSocket is where the deployment mounts the host's Docker socket.
//
// Deliberately not /var/run/docker.sock: dind runs its own daemon on that
// path, so the two would collide, and mounting it somewhere distinct makes the
// exclusion below a matter of intent rather than of coincidence.
const DefaultHostSocket = "/var/run/host-docker.sock"

// Plan works out what to launch. It is pure, so every rule below is testable
// without a daemon.
func Plan(self ContainerInfo, opts Options) (RunSpec, error) {
	if self.ID == "" && self.Name == "" {
		return RunSpec{}, fmt.Errorf("elevate: cannot identify this container")
	}
	if self.Image == "" {
		return RunSpec{}, fmt.Errorf("elevate: cannot determine this container's image")
	}
	if isElevated(self.Env) {
		return RunSpec{}, fmt.Errorf(
			"elevate: this container is already elevated (%s is set); "+
				"elevating again would fork containers indefinitely", ElevatedEnv)
	}

	hostSocket := opts.HostSocket
	if hostSocket == "" {
		hostSocket = DefaultHostSocket
	}

	// The child joins OUR network namespace. This is the whole reason the
	// scheme works: Swarm publishes the port into this task's namespace, and a
	// container started outside Swarm has no published port of its own. Get
	// this wrong and nothing can connect, with no error to explain why.
	target := self.ID
	if target == "" {
		target = self.Name
	}

	return RunSpec{
		Name:       childName(self.Name),
		Image:      self.Image,
		Network:    "container:" + target,
		Privileged: true,
		Remove:     true,
		Mounts:     childMounts(self.Mounts, hostSocket),
		Env:        childEnv(self.Env),
	}, nil
}

// childMounts replicates our mounts, minus the host's Docker socket.
//
// This exclusion is the difference between the design being safe and being a
// host takeover. A privileged container holding the host socket gives every
// enrolled workspace user root on the node -- they have access to the inner
// daemon by design, and the inner daemon would be able to reach the outer one.
//
// A blanket --volumes-from would have been shorter and would have carried the
// socket straight through, which is why the mounts are listed explicitly.
func childMounts(mounts []Mount, hostSocket string) []Mount {
	out := make([]Mount, 0, len(mounts))
	for _, m := range mounts {
		if isHostSocket(m, hostSocket) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// isHostSocket matches the socket by where it is mounted and by what it is, so
// a deployment that mounts it somewhere unexpected is still caught.
func isHostSocket(m Mount, hostSocket string) bool {
	if m.Destination == hostSocket {
		return true
	}
	return strings.HasSuffix(m.Destination, "/docker.sock") ||
		strings.HasSuffix(m.Source, "/docker.sock")
}

// childEnv passes our environment through, marking the child as elevated.
func childEnv(env []string) []string {
	out := make([]string, 0, len(env)+1)
	for _, e := range env {
		if strings.HasPrefix(e, ElevatedEnv+"=") {
			continue
		}
		out = append(out, e)
	}
	return append(out, ElevatedEnv+"=1")
}

// childName derives the child's name from ours. Docker reports container names
// with a leading slash.
func childName(name string) string {
	name = strings.TrimPrefix(name, "/")
	if name == "" {
		return ""
	}
	return name + NameSuffix
}

func isElevated(env []string) bool {
	for _, e := range env {
		if name, value, ok := strings.Cut(e, "="); ok && name == ElevatedEnv && value != "" {
			return true
		}
	}
	return false
}
