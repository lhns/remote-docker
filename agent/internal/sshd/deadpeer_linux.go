package sshd

import (
	"net"
	"syscall"
	"time"
)

// tcpUserTimeout is TCP_USER_TIMEOUT, which the syscall package does not name.
// It bounds how long data may stay unacknowledged before the socket fails,
// where SO_KEEPALIVE only probes a connection with nothing outstanding.
const tcpUserTimeout = 18

func setUserTimeout(tc *net.TCPConn, d time.Duration) {
	raw, err := tc.SyscallConn()
	if err != nil {
		return
	}
	_ = raw.Control(func(fd uintptr) {
		_ = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, tcpUserTimeout, int(d.Milliseconds()))
	})
}
