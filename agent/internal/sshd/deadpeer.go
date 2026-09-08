package sshd

// Deciding that a client is gone when it never said so.
//
// A reverse forward's port reservation is released when the connection ends
// (reversePolicy.Allow), which is only as good as this side's ability to notice
// an ending. A client whose network black-holes, a laptop suspended or a VPN
// dropped, leaves a connection that is dead and looks alive: Linux retransmits
// unacknowledged data for about fifteen minutes, and the port stays reserved
// for all of it.
//
// What that costs is worse than the lost connection. The client reconnects and
// is refused the one port its volumes can mount from ("tcpip-forward request
// denied by peer"), so it holds a session with no NFS export behind it and its
// containers get "connection refused" against a port bound to a corpse.
//
// The client probes the other direction itself (tunnelclient.keepAlive); this
// is the same judgement left to the kernel, because the agent has no
// request/reply clock of its own to hang one on. Covered by
// test/nfs-resilience.sh section 10.

import (
	"net"
	"time"
)

// peerTimeout is how long a connection may fail to make progress before the
// workspace treats the client as gone. Comparable to the client's own
// detection window (a 15s probe with a 30s wait): a workspace slower to decide
// than its client refuses the reconnect that client has already begun.
const peerTimeout = 60 * time.Second

// armDeadPeerDetection bounds how long a connection may go unanswered.
//
// Two mechanisms, because they cover different silences. Keepalives detect a
// peer that has gone while nothing was being sent; the user timeout covers the
// case where data IS in flight, which keepalives do not probe at all and which
// is exactly what a black-holed NFS reply looks like.
//
// Best effort by design: a connection this cannot be applied to still works,
// and it is the reservation's lifetime that suffers rather than the session.
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
