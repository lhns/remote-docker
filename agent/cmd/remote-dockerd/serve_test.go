package main

import (
	"net"
	"testing"
	"time"
)

// The agent is pid 1 in its container, so an SSH port it cannot bind has to
// end it, where the container's restart policy and log can say why. With the
// WebSocket listener up it hung instead: nothing closed the SSH server that
// listener was serving, and waiting for it never returned.
func TestServeReturnsWhenTheSSHPortIsTaken(t *testing.T) {
	t.Setenv(envEnableDind, "false")
	t.Setenv(envPerUserDind, "false")
	t.Setenv(envStateDir, t.TempDir())

	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = taken.Close() }()

	returned := make(chan error, 1)
	go func() { returned <- serve(taken.Addr().String(), "127.0.0.1:0") }()

	select {
	case err := <-returned:
		if err == nil {
			t.Error("serve returned no error for an SSH port it could not bind")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("serve hung after failing to bind its SSH port")
	}
}
