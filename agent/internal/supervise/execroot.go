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
// A container's /run is part of its writable layer, so runtime state written
// there outlives a kill, which on a real machine it never does. The dind
// entrypoint deletes `docker*.pid` and containerd's file is `containerd.pid`,
// so a workspace container that ended uncleanly and was started again on the
// SAME layer comes back with a stale containerd/containerd.pid naming a pid
// from its previous life. What that does to dockerd, both ways, is written
// down once in agent/internal/daemons.ExecRoot; a tmpfs is the same answer
// here (ADR 0012). README's "Restarting a workspace container" is the same
// thing said to an operator.
//
// Narrower than the per-account case: this daemon is stopped with SIGTERM and
// a clean shutdown removes the file, so it needs an unclean end AND a restart
// reusing the writable layer -- `docker restart`, a host reboot under
// `restart: unless-stopped`, an OOM kill, a SIGKILL after the stop grace
// period. Kubernetes is not exposed at all, because kubelet creates a
// container with a fresh layer.
//
// Done here rather than in deploy/docker-compose.yml, deploy/swarm.yml and the
// chart, which between them would fix nobody's existing deployment: the agent
// is privileged and can mount it for every deployment at once.
//
// Deleting a stale containerd.pid instead would be wrong. A SIGKILLed dockerd
// leaves its containerd reparented and possibly still running, and deleting
// the file then orphans it. The tmpfs is correct because it makes the whole
// directory not survive, which is what it does on a real machine.
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
// for any other reason, puts on screen. The daemon still starts; the only
// thing lost is that the directory survives a restart again.
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
		// than replacing it. Asked as st_dev against the parent, which is
		// core-agent/union's precedent for the same question.
		return false, "it already has a filesystem of its own"
	case serving(sockets):
		// THE case that must not be got wrong. Under
		// WORKSPACE_ENABLE_DIND=false the operator starts dockerd and the
		// agent may restart underneath it; a redeploy or a crash-restart can
		// leave one running here too. A fresh tmpfs over a serving daemon's
		// exec-root takes away its containerd socket, its shim sockets and its
		// runc state while it keeps running against paths nothing can reach,
		// so every container operation fails naming nothing. Strictly worse
		// than the stale pid file. Same discipline as a serving union, which
		// is adopted and never mounted over (ADR 0044).
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
