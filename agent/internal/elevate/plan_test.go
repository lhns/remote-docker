package elevate

import (
	"slices"
	"strings"
	"testing"
)

func self() ContainerInfo {
	return ContainerInfo{
		ID:    "abc123",
		Name:  "/workspace.1.xyz",
		Image: "ghcr.io/lhns/remote-docker-workspace:latest",
		Mounts: []Mount{
			{Type: "bind", Source: "/var/lib/remote-docker/state", Destination: "/etc/workspace"},
			{Type: "bind", Source: "/var/lib/remote-docker/authorized_keys.d", Destination: "/etc/workspace/authorized_keys.d", ReadOnly: true},
			{Type: "volume", Source: "workspace-docker", Destination: "/var/lib/docker"},
			{Type: "bind", Source: "/var/run/docker.sock", Destination: DefaultHostSocket},
		},
		Env: []string{"TZ=UTC", "WORKSPACE_DOCKERD_ARGS=--storage-driver=fuse-overlayfs"},
	}
}

// The child must join our network namespace. This is the whole reason the
// scheme works: Swarm publishes the port into this task's namespace, and a
// container started outside Swarm has no published port of its own. Getting it
// wrong means nothing can connect, with no error to explain why.
func TestPlanJoinsOurNetworkNamespace(t *testing.T) {
	spec, err := Plan(self(), Options{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if spec.Network != "container:abc123" {
		t.Errorf("Network = %q, want container:abc123", spec.Network)
	}
}

// The one that matters most. A privileged container holding the host socket
// gives every enrolled workspace user root on the node.
func TestPlanExcludesTheHostSocket(t *testing.T) {
	spec, err := Plan(self(), Options{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	for _, m := range spec.Mounts {
		if strings.Contains(m.Source, "docker.sock") || strings.Contains(m.Destination, "docker.sock") {
			t.Errorf("the host socket reached the privileged child: %+v", m)
		}
	}
}

// ...caught wherever it is mounted, not only at the expected path, so a
// deployment that mounts it somewhere else is still safe.
func TestPlanExcludesTheHostSocketAnywhere(t *testing.T) {
	for _, dest := range []string{
		DefaultHostSocket,
		"/var/run/docker.sock",
		"/tmp/docker.sock",
		"/somewhere/odd/docker.sock",
	} {
		info := self()
		info.Mounts = []Mount{{Type: "bind", Source: "/var/run/docker.sock", Destination: dest}}

		spec, err := Plan(info, Options{})
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if len(spec.Mounts) != 0 {
			t.Errorf("socket mounted at %q reached the child: %+v", dest, spec.Mounts)
		}
	}
}

// Everything else has to come through, or the workspace loses its state, its
// enrolled keys, or its docker graph.
func TestPlanKeepsEveryOtherMount(t *testing.T) {
	spec, err := Plan(self(), Options{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(spec.Mounts) != 3 {
		t.Fatalf("got %d mounts, want 3: %+v", len(spec.Mounts), spec.Mounts)
	}

	var destinations []string
	for _, m := range spec.Mounts {
		destinations = append(destinations, m.Destination)
	}
	for _, want := range []string{"/etc/workspace", "/etc/workspace/authorized_keys.d", "/var/lib/docker"} {
		if !slices.Contains(destinations, want) {
			t.Errorf("mount %q was dropped (kept %v)", want, destinations)
		}
	}

	// Read-only is part of the security posture of the keys directory.
	for _, m := range spec.Mounts {
		if m.Destination == "/etc/workspace/authorized_keys.d" && !m.ReadOnly {
			t.Error("the keys directory lost its read-only flag")
		}
	}
}

// Without a guard, a child that somehow started with the elevate command again
// would fork containers until the node fell over.
func TestPlanRefusesToElevateTwice(t *testing.T) {
	info := self()
	info.Env = append(info.Env, ElevatedEnv+"=1")

	if _, err := Plan(info, Options{}); err == nil {
		t.Fatal("an already-elevated container was elevated again")
	}
}

func TestPlanMarksTheChildAsElevated(t *testing.T) {
	spec, err := Plan(self(), Options{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !slices.Contains(spec.Env, ElevatedEnv+"=1") {
		t.Errorf("child env %v does not carry the elevation marker", spec.Env)
	}
	// The rest of the environment is what configures the workspace.
	if !slices.Contains(spec.Env, "WORKSPACE_DOCKERD_ARGS=--storage-driver=fuse-overlayfs") {
		t.Errorf("child env %v lost the workspace configuration", spec.Env)
	}
}

func TestPlanNamesTheChild(t *testing.T) {
	spec, err := Plan(self(), Options{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	// Docker reports names with a leading slash; the child's must not have one.
	if spec.Name != "workspace.1.xyz"+NameSuffix {
		t.Errorf("Name = %q, want %q", spec.Name, "workspace.1.xyz"+NameSuffix)
	}
	if !spec.Privileged {
		t.Error("the child is not privileged, which is the entire point")
	}
	if !spec.Remove {
		t.Error("the child is not --rm, so a restart would collide with the old one")
	}
	if spec.Image != self().Image {
		t.Errorf("Image = %q, want our own", spec.Image)
	}
}

// Swarm templates {{.Task.Name}} into an env var, so the name may be all we
// have; /proc parsing is only the fallback.
func TestPlanWorksFromANameAlone(t *testing.T) {
	info := self()
	info.ID = ""

	spec, err := Plan(info, Options{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if spec.Network != "container:/workspace.1.xyz" {
		t.Errorf("Network = %q, want it to target us by name", spec.Network)
	}
}

func TestPlanRejectsIncompleteSelf(t *testing.T) {
	tests := map[string]ContainerInfo{
		"no identity": {Image: "img"},
		"no image":    {ID: "abc"},
	}
	for name, info := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Plan(info, Options{}); err == nil {
				t.Error("an incomplete container was accepted")
			}
		})
	}
}

func TestPlanHonoursACustomHostSocketPath(t *testing.T) {
	info := self()
	info.Mounts = []Mount{
		{Type: "bind", Source: "/run/docker/docker.sock", Destination: "/custom/host.sock"},
		{Type: "bind", Source: "/data", Destination: "/data"},
	}

	spec, err := Plan(info, Options{HostSocket: "/custom/host.sock"})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(spec.Mounts) != 1 || spec.Mounts[0].Destination != "/data" {
		t.Errorf("mounts = %+v, want only /data", spec.Mounts)
	}
}

// Args must read Remove rather than assume it.
//
// It did not: `--rm` was appended unconditionally while the field was set and
// never consulted. Nothing caught it because nothing tested Args at all: the
// suite covered Plan's decisions thoroughly and the rendering not once, and
// the one caller happened to want the flag that was hardcoded.
//
// It matters now because the next caller is a per-user daemon. `--rm` on that
// would delete a user's daemon, and everything it was running, the moment it
// stopped.
func TestArgsHonoursRemove(t *testing.T) {
	kept := RunSpec{Image: "img", Remove: false}.Args()
	if slices.Contains(kept, "--rm") {
		t.Errorf("Remove=false still rendered --rm: %v", kept)
	}

	removed := RunSpec{Image: "img", Remove: true}.Args()
	if !slices.Contains(removed, "--rm") {
		t.Errorf("Remove=true did not render --rm: %v", removed)
	}
}

// elevate's own child is unchanged by that fix: it is a singleton whose state
// is worthless, and it should still go away when it stops.
func TestPlanStillAsksForARemovedChild(t *testing.T) {
	spec, err := Plan(ContainerInfo{ID: "abc", Name: "/ws", Image: "img"}, Options{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !spec.Remove {
		t.Error("the elevated child is no longer removed on exit")
	}
	if !slices.Contains(spec.Args(), "--rm") {
		t.Errorf("the elevated child's args lost --rm: %v", spec.Args())
	}
}

// The child is told which image it is, because it cannot find out.
//
// Asking means inspecting ourselves through the host's Docker socket, and
// keeping that socket out of the privileged child is the whole trust boundary.
// The agent needs the answer to give each account's daemon the same image as
// the workspace, the only one known to carry fuse-overlayfs, without which a
// Ceph- or NFS-backed workspace cannot start one at all.
func TestTheChildIsToldItsImage(t *testing.T) {
	spec, err := Plan(ContainerInfo{ID: "abc", Name: "/ws", Image: "ghcr.io/x/ws:sha-1"}, Options{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !slices.Contains(spec.Env, ImageEnv+"=ghcr.io/x/ws:sha-1") {
		t.Errorf("the child was not told its image: %v", spec.Env)
	}
}
