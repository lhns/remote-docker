//go:build windows

package proxy

import (
	"os"
	"path/filepath"
)

func lockDir() string {
	if dir, err := os.UserCacheDir(); err == nil {
		return filepath.Join(dir, "remote-docker")
	}
	return filepath.Join(os.TempDir(), "remote-docker")
}

func defaultEndpoint() string { return defaultPipe }

// acquireLock opens the pid record. The real exclusion is the pipe bind
// itself, which winio takes with FILE_FLAG_FIRST_PIPE_INSTANCE and the kernel
// releases when the process dies -- so unlike Unix there is nothing to recover
// from and nothing that can go stale.
//
// The file exists so `start` and `stop` can name the process that owns a
// workspace. Opened without O_EXCL for exactly that reason: refusing here
// would invent a failure the platform does not have.
//
// It does NOT write the pid, and that is the fix for a real and confusing bug.
// Opening is not winning here -- the bind that follows decides -- so writing
// now meant a process about to be REFUSED had already stamped its own pid over
// the owner's. The refusal read that back and reported "another remote-docker
// is already serving ... (pid <the failing process>)", sending the reader to
// look for a process that had just exited. On Unix the flock IS the exclusion,
// so writing on acquire is correct there; here the pid is recorded only once
// the pipe is bound.
func acquireLock(endpoint string) (*Lock, error) {
	path := LockPath(endpoint)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		// Not fatal: the pid record is a convenience, and the pipe bind that
		// follows is what actually decides.
		return &Lock{path: path}, nil
	}
	return &Lock{path: path, file: f}, nil
}

func (l *Lock) Release() {
	if l == nil || l.file == nil {
		return
	}
	_ = l.file.Close()
	l.file = nil
}
