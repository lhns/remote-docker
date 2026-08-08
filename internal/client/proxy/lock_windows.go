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

func defaultEndpoint() string { return DefaultEndpoint }

// acquireLock records the pid. The real exclusion is the pipe bind itself,
// which winio takes with FILE_FLAG_FIRST_PIPE_INSTANCE and the kernel releases
// when the process dies -- so unlike Unix there is nothing to recover from and
// nothing that can go stale.
//
// The file exists so `start` and `stop` can name the process that owns a
// workspace. Opened without O_EXCL for exactly that reason: refusing here
// would invent a failure the platform does not have.
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
	l := &Lock{path: path, file: f}
	l.writePid()
	return l, nil
}

func (l *Lock) Release() {
	if l == nil || l.file == nil {
		return
	}
	_ = l.file.Close()
	l.file = nil
}
