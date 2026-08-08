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

import "fmt"

// Path is where a process's network namespace can be opened.
//
// Untagged, unlike the rest of this package: it is string formatting, not a
// system call, and it is needed by callers that only want to NAME a namespace
// rather than enter one. It was declared once per build tag and a third time
// in internal/server/daemons -- three copies of one path format that have to
// agree for anything here to work.
func Path(pid int) string {
	return fmt.Sprintf("/proc/%d/ns/net", pid)
}
