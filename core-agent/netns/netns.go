// Package netns runs a function inside another process's network namespace:
// each account's dockerd has its own (ADR 0019), and the reverse tunnel
// carrying its NFS export is bound there, and published ports dialled from
// there, and nowhere else.
package netns

import (
	"fmt"
	"net"
)

// Do runs fn inside the network namespace named by path. An EMPTY path is this
// process's own, which is what lets the shared daemon (ADR 0012) and one per
// account (ADR 0019) be one code path with a different value.
func Do(path string, fn func() error) error {
	if path == "" {
		return fn()
	}
	return enter(path, fn)
}

// Listen binds a listener inside another network namespace. Only socket(2)
// reads the calling thread's namespace, so the listener is used from anywhere.
func Listen(path, network, address string) (net.Listener, error) {
	return do(path, func() (net.Listener, error) { return net.Listen(network, address) })
}

// Dial connects from inside another network namespace.
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
