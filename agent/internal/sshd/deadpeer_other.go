//go:build !linux

package sshd

import (
	"net"
	"time"
)

// The agent runs on Linux. This exists so the package still builds on the
// machine it is developed on, where the tests that do not need a workspace are
// run; keepalives alone are what a build here gets.
func setUserTimeout(*net.TCPConn, time.Duration) {}
