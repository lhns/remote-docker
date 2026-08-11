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

// Binding the endpoint is not a lock, and on one platform it is the opposite
// of one.
//
// On Windows a named pipe bind is exclusive: winio asks for
// FILE_FLAG_FIRST_PIPE_INSTANCE, and the kernel releases it when the process
// dies. On Unix, Listen removed any existing socket before binding, because a
// process that died without cleaning up would otherwise make every later run
// fail with "address already in use". That recovery is necessary and the way
// it was done was indiscriminate: a second process silently unlinked a running
// one's socket and took its place. The first kept accepting on an orphaned
// inode nobody could reach, and when the second exited the path was bound to
// nothing while the first still looked healthy.
//
// So: an explicit lock, held for as long as the endpoint is served. Taking it
// is what earns the right to clear a stale socket, which turns the theft into
// the recovery it was meant to be. The pid inside it is what lets `start` and
// `stop` say WHICH process owns a workspace.

// Lock is a held claim on one workspace's endpoint. Release when done.
type Lock struct {
	path string
	file *os.File
}

// LockPath is where the claim for an endpoint is recorded.
//
// Beside the endpoint rather than in the state directory, so the two cannot
// disagree about which workspace they describe: an explicitly configured
// endpoint is shared by every workspace pointed at it, and that is exactly the
// case a lock has to notice.
func LockPath(endpoint string) string {
	if endpoint == "" {
		endpoint = defaultEndpoint()
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

// Owner reads the pid recorded in a lock, or 0 if there is none.
//
// Advisory: the pid is for reporting, never for deciding whether the lock is
// held. Deciding is the lock's own job, because a pid can be reused.
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

// writePid records who holds the lock. Best effort: failing to write it costs
// a nicer error message later, never correctness.
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

// lockedListener releases the endpoint's claim when the listener closes, so
// the lock's lifetime is exactly the endpoint's and no caller has to remember.
type lockedListener struct {
	net.Listener
	lock *Lock
}

// closeRetry is how long to wait for a Close before signalling again, and how
// many times. Generous, because every attempt after the first is a symptom.
const (
	closeRetry    = 250 * time.Millisecond
	closeAttempts = 8
)

// Close releases the endpoint's claim, and keeps asking if the listener will
// not close.
//
// Asking twice is the fix rather than a workaround. go-winio's pipe listener
// signals its accept goroutine over an unbuffered channel and then waits to be
// told it finished (microsoft/go-winio#85, PR #369 unmerged as of 2026-08-11;
// re-check at github.com/microsoft/go-winio/issues/85). A client connecting at
// that exact moment can have the signal consumed by the connect path and
// reported back as ERROR_PIPE_CONNECTED or ERROR_NO_DATA, neither of which the
// caller recognises as "we are closing" -- so the signal is spent, the listener
// goes back to waiting for one, and Close blocks forever on a channel nobody
// will close. Accept never returns either, so the whole session hangs behind
// it. It presented as one CI run in many timing out after ten minutes.
//
// After swallowing a signal the listener is back in a select that will receive
// the next one, on both of the paths that can swallow it. So a second Close
// lands, the listener finishes, and every waiting Close returns together.
//
// Not conditioned on GOOS: a listener that closes promptly is closed on the
// first attempt and never reaches the timer, which is every listener on every
// other platform.
func (l *lockedListener) Close() error {
	defer l.lock.Release()

	// Buffered, so an attempt that finishes after we stop waiting does not
	// leak the goroutine holding it.
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
				// Give the caller its thread back. The endpoint stays bound
				// until the process exits, which for the background session is
				// immediately, and the lock is released by the defer either
				// way.
				return fmt.Errorf("proxy: the listener on %s did not close", l.Addr())
			}
			go closeOnce()
		}
	}
}
