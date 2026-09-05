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
)

// Do runs fn inside the network namespace named by path.
//
// An EMPTY path means this process's own namespace, and fn simply runs. That
// is not a convenience: it is what lets the shared-daemon mode (ADR 0012) and
// the per-account mode (ADR 0019) be the same code path with a different
// value, instead of an `if manager == nil` at every call site. Those branches
// were the thing most likely to route one account's traffic into another's
// namespace, because getting it wrong does not fail. It succeeds, somewhere
// else.
//
// It also means Listen and Dial work on the development machine for the shared
// case, where entering a NAMED namespace is unsupported.
func Do(path string, fn func() error) error {
	if path == "" {
		return fn()
	}
	return enter(path, fn)
}

// Listen binds a listener inside another network namespace.
//
// The listener is returned to the caller and used from wherever it likes; only
// the bind has to happen inside it: socket(2) reads the calling thread's
// namespace, and nothing afterwards does.
func Listen(path, network, address string) (net.Listener, error) {
	return do(path, func() (net.Listener, error) { return net.Listen(network, address) })
}

// Dial connects from inside another network namespace.
//
// The connection outlives the switch for the same reason a listener does: the
// namespace is decided when the socket is created.
func Dial(path, network, address string) (net.Conn, error) {
	return do(path, func() (net.Conn, error) { return net.Dial(network, address) })
}

// do is Do for a call that creates something.
func do[T any](path string, create func() (T, error)) (T, error) {
	var out T
	err := Do(path, func() error {
		var err error
		out, err = create()
		return err
	})
	if err != nil {
		var zero T
		return zero, err
	}
	return out, nil
}

// Path is where a process's network namespace can be opened.
//
// Untagged: it is string formatting, not a system call, and callers that only
// NAME a namespace need it too.
func Path(pid int) string {
	return fmt.Sprintf("/proc/%d/ns/net", pid)
}

// Root is a process's filesystem as seen from this one, which is how the agent
// reaches a path inside a daemon's mount namespace without entering it. Pid 0
// is this process's own, "/", for the shared daemon (ADR 0012).
func Root(pid int) string {
	if pid == 0 {
		return "/"
	}
	return fmt.Sprintf("/proc/%d/root", pid)
}
