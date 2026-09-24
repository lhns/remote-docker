package supervise

import (
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"
)

// prepareExecRoot puts a tmpfs on dockerd's exec-root before the daemon starts,
// for the shared daemon (ADR 0012), whose /run is a writable layer just as a
// per-account one's is (daemons.ExecRoot has the failure modes). Here rather
// than in the deploy files, so existing deployments get it too.
//
// Never deleting a stale containerd.pid instead: a SIGKILLed dockerd's
// containerd may still be running, and that orphans it. Never fatal either: a
// daemon that starts without the tmpfs beats one refused over a mount.
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
// The two refusals are the dangerous half, so they are testable anywhere.
func execRootAction(path string, sockets []string) (mount bool, why string) {
	switch {
	case path == "":
		return false, ""
	case mountedAt(path):
		// A second tmpfs would hide the first rather than replace it.
		return false, "it already has a filesystem of its own"
	case serving(sockets):
		// THE case that must not be got wrong: a tmpfs over a serving
		// daemon's exec-root takes its containerd, shim and runc state away
		// while it runs. Reachable under WORKSPACE_ENABLE_DIND=false, where
		// the agent restarts beneath the operator's dockerd.
		return false, "a docker daemon is already serving from it"
	}
	return true, ""
}

// containerdSocket is where a daemon serving from this exec-root listens for
// its containerd. Dialled rather than stat'ed: a socket file outlives its
// process.
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
