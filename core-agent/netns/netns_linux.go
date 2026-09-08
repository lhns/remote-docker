//go:build linux

package netns

import (
	"fmt"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

// enter runs fn inside the network namespace named by path, then restores this
// thread's own. Callers reach it through Do, which handles the empty path.
//
// `socket(2)` uses the CALLING THREAD's network namespace, not the process's,
// and Go schedules goroutines across threads freely, so the switch and the
// socket call have to happen on one pinned thread. Once the fd exists it
// belongs to that namespace and works from any thread, which is why this
// package wraps the creating call and not the whole connection.
//
// If restoring fails, the thread is DELIBERATELY never unlocked. An unlocked
// thread goes back to the runtime's pool still sitting in someone else's
// namespace, and the next goroutine scheduled onto it opens sockets there,
// invisibly. Go retires a locked thread when its goroutine exits, so leaking
// one is the cheap and correct answer to a problem with no safe recovery.
func enter(path string, fn func() error) error {
	self, err := os.Open(Path(os.Getpid()))
	if err != nil {
		return fmt.Errorf("netns: opening our own namespace: %w", err)
	}
	defer func() { _ = self.Close() }()

	target, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("netns: opening %s: %w", path, err)
	}
	defer func() { _ = target.Close() }()

	// Buffered, because the goroutine below may be parked forever on the
	// restore-failure path and must not also block on a send.
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread()

		if err := unix.Setns(int(target.Fd()), unix.CLONE_NEWNET); err != nil {
			runtime.UnlockOSThread()
			done <- fmt.Errorf("netns: entering %s: %w", path, err)
			return
		}

		err := fn()

		if rerr := unix.Setns(int(self.Fd()), unix.CLONE_NEWNET); rerr != nil {
			// Park. See above: returning this thread to the pool would leak
			// another namespace into unrelated work.
			done <- fmt.Errorf("netns: could not return this thread from %s: %w", path, rerr)
			select {}
		}

		runtime.UnlockOSThread()
		done <- err
	}()

	return <-done
}
