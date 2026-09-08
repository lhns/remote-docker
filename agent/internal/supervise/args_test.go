package supervise

import "testing"

// The first word decides whether dind's entrypoint adds
// --host=tcp://0.0.0.0:2375 on top of whatever we asked for. It supplies its
// own flags only when the first argument is absent or starts with a dash, so
// an argument list of flags alone -- which is what WORKSPACE_DOCKERD_ARGS is
// -- puts an unauthenticated Docker API in the namespace every shell of this
// workspace runs in.
func TestTheDaemonIsNamedSoTheEntrypointAddsNoTCPListener(t *testing.T) {
	d := &Dockerd{Socket: "/var/run/docker.sock", Args: []string{"--storage-driver=fuse-overlayfs"}}

	args := d.args()
	if args[0] != "dockerd" {
		t.Errorf("the entrypoint chooses the listeners when the first argument is %q", args[0])
	}
	if want := "--host=unix:///var/run/docker.sock"; args[1] != want {
		t.Errorf("args[1] = %q, want %q", args[1], want)
	}
	if last := args[len(args)-1]; last != "--storage-driver=fuse-overlayfs" {
		t.Errorf("WORKSPACE_DOCKERD_ARGS did not survive: %v", args)
	}
}

// WaitReady watches one path and the daemon must listen on that same one. The
// entrypoint would have derived it from DOCKER_HOST, which is a second place
// for the two to disagree.
func TestTheDaemonListensWhereWaitReadyWatches(t *testing.T) {
	d := &Dockerd{Socket: "/run/elsewhere/docker.sock"}

	if got := d.args()[1]; got != "--host=unix:///run/elsewhere/docker.sock" {
		t.Errorf("the daemon does not listen where WaitReady watches: %q", got)
	}
}
