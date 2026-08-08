//go:build linux

// Package netns runs a function inside another process's network namespace.
//
// The agent needs this because each user's dockerd lives in its own network
// namespace (ADR 0019). Two things have to cross that boundary: the reverse
// tunnel the client's NFS export answers on, which must be reachable from
// inside the user's daemon and from nowhere else, and a local forward to a
// published port, which must be dialled from inside it.
//
// The alternatives were both worse. Joining the agent's own namespace
// (`--network container:<agent>`) puts two dockerds on one bridge, collides
// every user's published ports, and lands them all in the namespace where
// every user's shell runs. Giving the agent an address on a per-user bridge
// network means relaxing the loopback rule in forward.go, which is the single
// thing standing between an unauthenticated NFS export and everybody --
// docker's isolation blocks bridge-to-bridge traffic, not container-to-host.
package netns

import (
	"fmt"
	"net"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

// Path is where a process's network namespace can be opened.
func Path(pid int) string {
	return fmt.Sprintf("/proc/%d/ns/net", pid)
}

// Do runs fn inside the network namespace named by path, then restores this
// thread's own.
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
func Do(path string, fn func() error) error {
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

// Listen binds a listener inside another network namespace.
//
// The listener is returned to the caller and used from wherever it likes; only
// the bind has to happen inside.
func Listen(path, network, address string) (net.Listener, error) {
	var (
		l   net.Listener
		err error
	)
	if derr := Do(path, func() error {
		l, err = net.Listen(network, address)
		return err
	}); derr != nil {
		return nil, derr
	}
	return l, nil
}

// Dial connects from inside another network namespace.
//
// The connection outlives the switch for the same reason a listener does: the
// namespace is decided when the socket is created.
func Dial(path, network, address string) (net.Conn, error) {
	var (
		c   net.Conn
		err error
	)
	if derr := Do(path, func() error {
		c, err = net.Dial(network, address)
		return err
	}); derr != nil {
		return nil, derr
	}
	return c, nil
}
