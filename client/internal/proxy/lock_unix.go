//go:build !windows

package proxy

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func lockDir() string { return filepath.Dir(defaultSocketPath()) }

// acquireLock takes an exclusive, non-blocking flock.
//
// flock rather than a pid file with a liveness check, because the kernel
// releases it when the holder dies, including when it is killed, which a
// pid file cannot survive correctly. A stale lock is therefore impossible.
func acquireLock(endpoint string) (*Lock, error) {
	path := LockPath(endpoint)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		pid := Owner(endpoint)
		_ = f.Close()
		return nil, &ErrLocked{Endpoint: endpoint, PID: pid}
	}
	l := &Lock{path: path, file: f}
	l.writePid()
	return l, nil
}

// Release drops the claim. The file stays: it is the pid record, and removing
// it would race another process that has already opened it.
func (l *Lock) Release() {
	if l == nil || l.file == nil {
		return
	}
	_ = unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	_ = l.file.Close()
	l.file = nil
}
