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

// acquireLock only opens the pid record: the pipe bind is the exclusion here.
// It must not write the pid, or a process about to be refused overwrites the
// owner's and the refusal names the wrong process; Listen writes it once bound.
func acquireLock(endpoint string) (*Lock, error) {
	path := LockPath(endpoint)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		// Not fatal: the record is a convenience.
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
