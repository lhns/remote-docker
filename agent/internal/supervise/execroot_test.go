package supervise

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// A serving daemon's exec-root is left alone. Mounting a fresh tmpfs over it
// takes away the containerd socket, the shim sockets and the runc state of a
// daemon that keeps running, which is worse than the stale containerd.pid this
// is fixing.
func TestAServingDaemonsExecRootIsNotMountedOver(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "docker.sock")
	listen(t, sock)

	mount, why := execRootAction(dir, []string{sock, containerdSocket(dir)})
	if mount {
		t.Fatalf("would mount a tmpfs over a daemon that is serving on %s", sock)
	}
	if why == "" {
		t.Error("declined without saying why, so the log line names nothing")
	}
}

// The containerd socket inside the exec-root answers the same question, and is
// the one that catches a dockerd the agent did not start and cannot see on its
// own socket path.
func TestTheContainerdSocketAlsoCounts(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "containerd"), 0o755); err != nil {
		t.Fatal(err)
	}
	listen(t, containerdSocket(dir))

	if mount, _ := execRootAction(dir, []string{"", containerdSocket(dir)}); mount {
		t.Error("would mount a tmpfs over a live containerd")
	}
}

// A socket file left behind by a process that is gone is not a daemon. It is
// dialled rather than stat'ed for exactly this: dockerd does not remove its
// sockets when it is killed.
func TestASocketNothingListensOnIsNotADaemon(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "docker.sock")
	listen(t, sock)()

	if mount, why := execRootAction(dir, []string{sock}); !mount {
		t.Errorf("declined a dead socket: %s", why)
	}
}

func TestNoExecRootMeansNoMountAndNothingLogged(t *testing.T) {
	mount, why := execRootAction("", []string{"/nonexistent.sock"})
	if mount {
		t.Error("would mount a tmpfs on an empty path")
	}
	if why != "" {
		t.Errorf("an unset exec-root is not a decision to report: %q", why)
	}
}

// listen binds a unix socket and returns a function that closes it, so a test
// can also ask what happens once nothing is listening.
func listen(t *testing.T, path string) func() {
	t.Helper()
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Skipf("no unix sockets here: %v", err)
	}
	closed := false
	stop := func() {
		if !closed {
			closed = true
			_ = l.Close()
		}
	}
	t.Cleanup(stop)
	return stop
}
