package sshd

// Deciding that a client is gone when it never said so.
//
// A reverse forward's port reservation is released when the connection ends
// (see reversePolicy.Allow), and that promise is only as good as the workspace's
// ability to notice an ending. Nothing here ever did. A client whose network
// black-holes -- a laptop suspended, a VPN dropped, a route withdrawn -- leaves
// a connection that is dead and looks alive: with unacknowledged data on it,
// Linux retransmits for something like fifteen minutes before the socket fails,
// and until it does the port stays reserved.
//
// The client is then refused the only port its volumes can use, on every
// reconnect, with "tcpip-forward request denied by peer (another session for
// this account may still be open)" -- and the session it does get has no NFS
// export behind it, so containers mount nothing and read "connection refused"
// against a port that is bound to a corpse.
//
// The client already probes this way round (tunnelclient.keepAlive); this is
// the same judgement made by the kernel on the workspace's side, because the
// agent has no request/reply clock of its own to hang one on.
//
// Measured in test/nfs-resilience.sh section 10: before this, no docker command
// worked for the eight minutes the suite was willing to wait.

import (
	"net"
	"time"
)

// peerTimeout is how long a connection may fail to make progress before the
// workspace treats the client as gone.
//
// Comparable to the client's own detection window (a 15s probe with a 30s
// wait), because the two are answering the same question from opposite ends and
// a workspace slower to decide than its client is a workspace that refuses the
// reconnect the client has already begun.
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
