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
// The discipline here is the whole package, and getting it wrong is silent.
//
// `socket(2)` uses the CALLING THREAD's network namespace, not the process's.
// Go schedules goroutines across threads freely, so the switch and the socket
// call have to happen on one pinned thread -- hence LockOSThread. Once the fd
// exists it is an ordinary fd belonging to that namespace, and Accept, Read
// and Write work from any thread afterwards. That asymmetry is why this
// package only has to wrap the creating call and not the whole connection.
//
// If restoring fails, the thread is DELIBERATELY never unlocked. An unlocked
// thread goes back to the runtime's pool still sitting in someone else's
// namespace, and the next goroutine scheduled onto it opens sockets there --
// arbitrarily, invisibly, and in a user's private namespace. Go retires a
// locked thread when its goroutine exits, so leaking one thread is the cheap
// and correct answer to a problem that has no safe recovery.
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
