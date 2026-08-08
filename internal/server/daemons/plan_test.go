package daemons

import (
	"slices"
	"strings"
	"testing"
)

func plan(t *testing.T, account string, opts Options) Spec {
	t.Helper()
	spec, err := Plan(account, opts)
	if err != nil {
		t.Fatalf("Plan(%q): %v", account, err)
	}
	return spec
}

// The one that would be catastrophic.
//
// A user's daemon holds their running containers, their images and their
// volumes. `--rm` would delete all of it the moment the daemon stopped -- on a
// restart, on an OOM kill, on a redeploy -- and the user would have no idea
// why their work evaporated. elevate's child DOES take --rm because it is a
// singleton whose state is worthless; copying that here would be the single
// worst mistake available in this package.
func TestADaemonIsNeverRemovedOnExit(t *testing.T) {
	spec := plan(t, "alice", Options{})
	if spec.Remove {
		t.Error("Remove is set; a user's daemon would be deleted when it stopped")
	}
	if args := spec.Args(); slices.Contains(args, "--rm") {
		t.Errorf("--rm rendered: %v", args)
	}
}

// Each account's daemon must be findable again after the agent restarts,
// before anything has read its labels.
func TestNamesAndVolumesAreDerivedFromTheAccount(t *testing.T) {
	a := plan(t, "alice", Options{})
	b := plan(t, "bob", Options{})

	if a.Name == b.Name {
		t.Errorf("two accounts share a container name: %q", a.Name)
	}
	if VolumeName("alice") == VolumeName("bob") {
		t.Error("two accounts share a graph volume; one would see the other's images")
	}
	if SocketPathFor("alice") == SocketPathFor("bob") {
		t.Error("two accounts share a socket path")
	}
}

// Adoption keys on a workspace id persisted in the state directory, never on a
// container id: an id changes on every redeploy, so adopting by it would
// orphan every user's daemon the first time somebody ran `compose up -d`.
func TestLabelsCarryTheAccountAndTheWorkspace(t *testing.T) {
	spec := plan(t, "alice", Options{Workspace: "ws-123"})

	want := []string{
		ManagedLabel + "=1",
		AccountLabel + "=alice",
		WorkspaceLabel + "=ws-123",
	}
	for _, l := range want {
		if !slices.Contains(spec.Labels, l) {
			t.Errorf("label %q missing from %v", l, spec.Labels)
		}
	}

	// And they must reach the command line, or adoption finds nothing.
	args := strings.Join(spec.Args(), " ")
	for _, l := range want {
		if !strings.Contains(args, l) {
			t.Errorf("label %q never reached the args: %s", l, args)
		}
	}
}

// A workspace with no persisted id yet must still produce a usable spec rather
// than a label with an empty value, which would match every other daemon whose
// id was also unset.
func TestAnUnknownWorkspaceOmitsTheLabelRatherThanEmptyingIt(t *testing.T) {
	spec := plan(t, "alice", Options{})
	for _, l := range spec.Labels {
		if strings.HasPrefix(l, WorkspaceLabel+"=") {
			t.Errorf("workspace label present with no id: %q", l)
		}
	}
}

// The agent dials the socket in the bind-mounted directory; the conventional
// path is kept so anything running inside the daemon still finds one.
func TestTheDaemonListensWhereTheAgentDials(t *testing.T) {
	spec := plan(t, "alice", Options{})
	command := strings.Join(spec.Command, " ")

	if !strings.Contains(command, "unix://"+SocketMount+"/"+SocketName) {
		t.Errorf("the daemon does not listen where the agent dials: %s", command)
	}
	if !strings.Contains(command, "unix:///var/run/docker.sock") {
		t.Errorf("the conventional socket is gone: %s", command)
	}

	// The bind must put the agent's per-account directory at that mount point,
	// or the socket the daemon creates is invisible to the agent.
	var bound bool
	for _, m := range spec.Mounts {
		if m.Destination == SocketMount && m.Source == SocketDir+"/alice" {
			bound = true
		}
	}
	if !bound {
		t.Errorf("the account's socket directory is not bound: %+v", spec.Mounts)
	}
}

// Never /var/run: binding over it inside the dind hides containerd's own
// sockets, and the daemon fails in a way that names neither.
func TestTheSocketBindDoesNotCoverVarRun(t *testing.T) {
	spec := plan(t, "alice", Options{})
	for _, m := range spec.Mounts {
		if m.Destination == "/var/run" || m.Destination == "/run" {
			t.Errorf("bind covers %s, which would hide containerd's sockets", m.Destination)
		}
	}
}

// The graph volume is mounted by name. Rendering it as a host path instead
// bypasses the volume and binds the daemon's internal storage directory --
// which happens to work, and is quietly wrong.
func TestTheGraphVolumeIsMountedByName(t *testing.T) {
	spec := plan(t, "alice", Options{})
	if !slices.Contains(spec.Args(), VolumeName("alice")+":/var/lib/docker") {
		t.Errorf("the graph volume is not mounted by name: %v", spec.Args())
	}
}

// A deployment on a Ceph-backed graph volume sets fuse-overlayfs, and it is
// not inherited -- each account's daemon has to be told.
func TestTheStorageDriverIsPropagated(t *testing.T) {
	spec := plan(t, "alice", Options{StorageDriver: "fuse-overlayfs"})
	command := strings.Join(spec.Command, " ")
	if !strings.Contains(command, "--storage-driver fuse-overlayfs") {
		t.Errorf("storage driver not propagated: %s", command)
	}

	if plain := plan(t, "alice", Options{}); strings.Contains(strings.Join(plain.Command, " "), "--storage-driver") {
		t.Errorf("a storage driver appeared when none was asked for: %v", plain.Command)
	}
}

func TestPlanRejectsAnUnusableAccount(t *testing.T) {
	for _, account := range []string{"", "a/b", "a b"} {
		if _, err := Plan(account, Options{}); err == nil {
			t.Errorf("Plan(%q) was accepted; it would name a container that is not one", account)
		}
	}
}

func TestTheImageIsOverridable(t *testing.T) {
	if got := plan(t, "alice", Options{}).Image; got != DefaultImage {
		t.Errorf("default image = %q", got)
	}
	if got := plan(t, "alice", Options{Image: "docker:27-dind"}).Image; got != "docker:27-dind" {
		t.Errorf("image override ignored: %q", got)
	}
}

// Adoption reads docker's comma-separated label rendering, which is the only
// form `docker ps --format` offers.
func TestLabelValue(t *testing.T) {
	labels := ManagedLabel + "=1," + AccountLabel + "=alice," + WorkspaceLabel + "=ws-1"

	if got := labelValue(labels, AccountLabel); got != "alice" {
		t.Errorf("account = %q", got)
	}
	if got := labelValue(labels, WorkspaceLabel); got != "ws-1" {
		t.Errorf("workspace = %q", got)
	}
	// A label that is merely a PREFIX of another must not match: adopting
	// "remote-docker.account" by matching "remote-docker.acc" would hand one
	// user's daemon to another.
	if got := labelValue(labels, "remote-docker.acc"); got != "" {
		t.Errorf("a prefix matched: %q", got)
	}
	if got := labelValue("", AccountLabel); got != "" {
		t.Errorf("empty labels produced %q", got)
	}
}

// The workspace being restarted is the moment a daemon most needs to come
// back, and the parent dockerd restarts only containers that asked to.
//
// Found in CI rather than by reasoning: after a workspace restart every
// account's daemon stayed down until that account next connected, so adoption
// found nothing to adopt and a detached container -- which survives its
// author's disconnect by design -- did not survive the restart.
func TestADaemonAsksToBeRestarted(t *testing.T) {
	spec := plan(t, "alice", Options{})

	if spec.Restart != "unless-stopped" {
		t.Errorf("Restart = %q, want unless-stopped", spec.Restart)
	}
	// Not "always": an operator who deliberately stopped somebody's daemon
	// should find it still stopped.
	if slices.Contains(spec.Args(), "always") {
		t.Errorf("the restart policy is always: %v", spec.Args())
	}

	args := strings.Join(spec.Args(), " ")
	if !strings.Contains(args, "--restart unless-stopped") {
		t.Errorf("the policy never reached the args: %s", args)
	}
}

// A per-account daemon does not inherit its parent's flags, and the one that
// matters is the graph driver.
//
// This is the bug that shipped: a deployment on Ceph-backed storage sets
// --storage-driver=fuse-overlayfs for the workspace's own dockerd because
// overlay2 refuses that filesystem, the per-account daemon did not inherit it,
// and dockerd fell back to VFS -- which copies the entire image on every
// container create. `docker ps` stayed instant, `docker create debian` took 90
// to 113 seconds, and nothing anywhere said why, because nothing had failed.
func TestTheStorageDriverIsInheritedFromTheParent(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--storage-driver=fuse-overlayfs"}, "fuse-overlayfs"},
		{[]string{"--storage-driver", "fuse-overlayfs"}, "fuse-overlayfs"},
		{[]string{"--debug", "--storage-driver=vfs", "--iptables=false"}, "vfs"},
		{[]string{"--debug"}, ""},
		{nil, ""},
		// A trailing --storage-driver with nothing after it is a malformed
		// argument, not an empty driver: reading past the end would panic.
		{[]string{"--storage-driver"}, ""},
	} {
		if got := StorageDriverFrom(tc.args); got != tc.want {
			t.Errorf("StorageDriverFrom(%q) = %q, want %q", tc.args, got, tc.want)
		}
	}
}
