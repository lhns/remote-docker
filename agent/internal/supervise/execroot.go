package supervise

import (
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"
)

// prepareExecRoot puts a tmpfs on dockerd's exec-root before the daemon starts.
//
// The shared daemon (ADR 0012) has the exposure daemons.ExecRoot describes, for
// the same reason: it runs the same entrypoint in the workspace container,
// whose /run is equally a writable layer. Narrower, because this daemon is
// stopped with SIGTERM and a clean shutdown removes the file, so it needs an
// unclean end AND a restart reusing the layer: `docker restart`, a host reboot
// under `restart: unless-stopped`, an OOM kill, a SIGKILL after the stop grace
// period. Kubernetes is not exposed at all, because kubelet creates a container
// with a fresh layer. README's "Restarting a workspace container" says the same
// to an operator.
//
// Done here rather than in deploy/docker-compose.yml, deploy/swarm.yml and the
// chart, which between them would fix nobody's existing deployment: the agent
// is privileged and can mount it for every deployment at once.
//
// Deleting a stale containerd.pid instead would be wrong: a SIGKILLed dockerd
// leaves its containerd reparented and possibly still running, and deleting the
// file orphans it. The tmpfs is correct because it makes the whole directory
// not survive, which is what it does on a real machine.
//
// Never fatal: a workspace serving without the tmpfs has the exposure it had
// before this existed, and refusing to start the daemon over a failed mount is
// worse than the bug.
func prepareExecRoot(path, socket string, log *slog.Logger) {
	mount, why := execRootAction(path, []string{socket, containerdSocket(path)})
	if !mount {
		if why != "" {
			log.Info("leaving dockerd's exec-root as it is", "path", path, "why", why)
		}
		return
	}

	if err := os.MkdirAll(path, 0o755); err != nil {
		warnNoTmpfs(log, path, err)
		return
	}
	if err := mountTmpfs(path); err != nil {
		warnNoTmpfs(log, path, err)
		return
	}
	log.Info("mounted a tmpfs on dockerd's exec-root", "path", path)
}

// warnNoTmpfs is what an agent that is not privileged, or whose mount fails
// for any other reason, puts on screen.
func warnNoTmpfs(log *slog.Logger, path string, err error) {
	log.Warn("no tmpfs on dockerd's exec-root; a stale containerd.pid there can stop the daemon starting after an unclean kill",
		"path", path, "err", err)
}

// execRootAction decides whether to mount, and says why not when it declines.
//
// Separated from the mounting so the two refusals can be tested on a machine
// with no Linux: both of them are the dangerous half of this change.
func execRootAction(path string, sockets []string) (mount bool, why string) {
	switch {
	case path == "":
		return false, ""
	case mountedAt(path):
		// An operator may have mounted one, and a redeploy may have left the
		// agent's own. A second tmpfs on the same path hides the first rather
		// than replacing it.
		return false, "it already has a filesystem of its own"
	case serving(sockets):
		// THE case that must not be got wrong. Under
		// WORKSPACE_ENABLE_DIND=false the operator starts dockerd and the
		// agent may restart underneath it. A fresh tmpfs over a serving
		// daemon's exec-root takes away its containerd socket, its shim
		// sockets and its runc state while it keeps running, so every
		// container operation fails naming nothing: strictly worse than the
		// stale pid file. Same discipline as a serving union, which is adopted
		// and never mounted over (ADR 0044).
		return false, "a docker daemon is already serving from it"
	}
	return true, ""
}

// containerdSocket is where a daemon serving from this exec-root listens for
// its containerd. Its presence proves nothing -- a socket file outlives the
// process that bound it -- so it is dialled rather than stat'ed.
func containerdSocket(execRoot string) string {
	if execRoot == "" {
		return ""
	}
	return filepath.Join(execRoot, "containerd", "containerd.sock")
}

// serving reports whether anything accepts on any of these unix sockets.
func serving(sockets []string) bool {
	for _, s := range sockets {
		if s == "" {
			continue
		}
		c, err := net.DialTimeout("unix", s, 2*time.Second)
		if err != nil {
			continue
		}
		_ = c.Close()
		return true
	}
	return false
}
