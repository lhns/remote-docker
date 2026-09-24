package sshd

// Deciding that a client is gone when it never said so. A black-holed client
// looks alive for the ~15 minutes Linux retransmits, and its reverse-tunnel
// port stays reserved all that time, so its reconnect is refused its own
// forward and its containers mount against a port bound to nothing. The client
// probes the other direction itself (tunnelclient.keepAlive). Covered by
// test/nfs-resilience.sh section 10.

import (
	"net"
	"time"
)

// peerTimeout is how long a connection may fail to make progress before the
// client is treated as gone. Comparable to the client's own window (a 15s
// probe, a 30s wait): a workspace slower to decide refuses the reconnect its
// client has already begun.
const peerTimeout = 60 * time.Second

// armDeadPeerDetection bounds how long a connection may go unanswered.
// Keepalives cover a silence with nothing in flight; the user timeout covers
// unacknowledged data, which is what a black-holed NFS reply is and which
// keepalives never probe. Best effort: a connection it cannot apply to still
// works.
func armDeadPeerDetection(conn net.Conn) {
	tc, ok := conn.(*net.TCPConn)
	if !ok {
		return
	}

	_ = tc.SetKeepAliveConfig(net.KeepAliveConfig{
		Enable:   true,
		Idle:     peerTimeout / 2,
		Interval: peerTimeout / 6,
		Count:    3,
	})
	setUserTimeout(tc, peerTimeout)
}
