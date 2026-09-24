package proxy

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Binding the endpoint is not a lock (ADR 0017). On Unix a bind excludes
// nothing, and clearing a stale socket unlinks a RUNNING process's socket,
// which keeps accepting on an inode nobody can reach. So the lock is held for
// as long as the endpoint is served, and only its holder clears a socket. On
// Windows the pipe bind excludes and the file only records the pid.

// Lock is a held claim on one workspace's endpoint. Release when done.
type Lock struct {
	path string
	file *os.File
}

// LockPath is keyed on the endpoint, not the workspace: two workspaces
// configured with one endpoint must contend for one lock.
func LockPath(endpoint string) string {
	if endpoint == "" {
		endpoint = DefaultEndpoint()
	}
	return filepath.Join(lockDir(), sanitizeLockName(endpoint)+".lock")
}

// sanitizeLockName turns an endpoint into a single filename component.
func sanitizeLockName(endpoint string) string {
	name := strings.TrimPrefix(endpoint, `\\.\pipe\`)
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "endpoint"
	}
	return b.String()
}

// Owner reads the pid recorded in a lock, or 0. For reporting only: a pid can
// be reused, so it never decides whether the lock is held.
func Owner(endpoint string) int {
	data, err := os.ReadFile(LockPath(endpoint))
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return pid
}

// writePid records who holds the lock. Best effort.
func (l *Lock) writePid() {
	if l == nil || l.file == nil {
		return
	}
	_ = l.file.Truncate(0)
	_, _ = l.file.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
}

// ErrLocked reports that another process is already serving this endpoint.
type ErrLocked struct {
	Endpoint string
	PID      int
}

func (e *ErrLocked) Error() string {
	if e.PID > 0 {
		return fmt.Sprintf("another remote-docker is already serving %s (pid %d)", e.Endpoint, e.PID)
	}
	return fmt.Sprintf("another remote-docker is already serving %s", e.Endpoint)
}

// lockedListener releases the endpoint's lock when the listener closes.
type lockedListener struct {
	net.Listener
	lock *Lock
}

const (
	closeRetry    = 250 * time.Millisecond
	closeAttempts = 8
)

// Close retries because go-winio's pipe listener can lose its close signal to
// a client connecting at that moment (ERROR_PIPE_CONNECTED / ERROR_NO_DATA),
// leaving Close and Accept blocked forever; a second signal lands.
// microsoft/go-winio#85, PR #369 unmerged as of 2026-08-11; re-check there.
// Harmless elsewhere: a prompt close never reaches the timer.
func (l *lockedListener) Close() error {
	defer l.lock.Release()

	// Buffered, so a late attempt does not leak its goroutine.
	done := make(chan error, closeAttempts)
	closeOnce := func() { done <- l.Listener.Close() }
	go closeOnce()

	for attempt := 1; ; attempt++ {
		timer := time.NewTimer(closeRetry)
		select {
		case err := <-done:
			timer.Stop()
			return err
		case <-timer.C:
			if attempt >= closeAttempts {
				// Give up; the endpoint stays bound until the process exits.
				return fmt.Errorf("proxy: the listener on %s did not close", l.Addr())
			}
			go closeOnce()
		}
	}
}
