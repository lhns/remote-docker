package main

import (
	"context"
	"net"
	"strings"
	"testing"
)

// The check has to FAIL when nothing is listening, or it is the `nc -z` it
// replaced: something that passes while the workspace is unusable.
func TestHealthcheckFailsWhenSSHIsNotAccepting(t *testing.T) {
	err := sshAccepting(context.Background(), ":65534")
	if err == nil {
		t.Fatal("a port nothing listens on was reported healthy")
	}
	// And it must name the port, since the whole point is a reason.
	if !strings.Contains(err.Error(), "65534") {
		t.Errorf("the error does not name the port: %v", err)
	}
}

func TestHealthcheckPassesWhenSSHIsAccepting(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = l.Close() }()

	_, port, _ := net.SplitHostPort(l.Addr().String())
	if err := sshAccepting(context.Background(), ":"+port); err != nil {
		t.Errorf("a listening port was reported unhealthy: %v", err)
	}
}

// A bind address is not a dial address: the agent serves ":2222", and the
// check has to turn that into loopback rather than dialling an empty host.
func TestHealthcheckDialsLoopbackForABareBindAddress(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = l.Close() }()

	_, port, _ := net.SplitHostPort(l.Addr().String())
	for _, addr := range []string{":" + port, "0.0.0.0:" + port, "127.0.0.1:" + port} {
		if err := sshAccepting(context.Background(), addr); err != nil {
			t.Errorf("addr %q was not resolved to loopback: %v", addr, err)
		}
	}
}
