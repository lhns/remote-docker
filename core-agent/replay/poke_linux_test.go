//go:build linux

package replay

import (
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A FIFO in a share is somebody's ordinary file, and opening one for writing
// blocks until a reader appears. The notify channel replays one event at a
// time, so a poke that waited there wedged every event after it.
func TestPokeDoesNotWaitOnAFifo(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "pipe")
	if err := unix.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("cannot make a fifo here: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- SyscallPoker{}.Poke(fifo, false) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("poking a fifo blocked waiting for a reader")
	}
}
