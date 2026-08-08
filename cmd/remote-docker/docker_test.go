package main

import (
	"os"
	"testing"
)

// The docker subtree is built on EVERY invocation, because cobra assembles the
// whole command tree before parsing anything. It used to probe the endpoint
// and could open a file-serving session while building -- so `remote-docker
// gc` raced its own session, and `--help` reached for the network.
func TestInvokingDocker(t *testing.T) {
	tests := []struct {
		args []string
		want bool
	}{
		{[]string{"remote-docker", "docker", "ps"}, true},
		{[]string{"remote-docker", "docker"}, true},
		{[]string{"remote-docker", "gc"}, false},
		{[]string{"remote-docker", "status"}, false},
		{[]string{"remote-docker", "--help"}, false},
		{[]string{"remote-docker"}, false},

		// A flag's value is not the subcommand.
		{[]string{"remote-docker", "--workspace", "docker", "gc"}, false},
		{[]string{"remote-docker", "--workspace", "dev", "docker", "ps"}, true},
		{[]string{"remote-docker", "--workspace=dev", "docker", "ps"}, true},
		{[]string{"remote-docker", "--workspace=docker", "gc"}, false},
		{[]string{"remote-docker", "--port", "2222", "docker", "ps"}, true},

		// A boolean-looking flag we do not know still must not swallow the
		// subcommand, because guessing wrong here is what this test exists for.
		{[]string{"remote-docker", "--verbose", "docker", "ps"}, true},
	}

	saved := os.Args
	t.Cleanup(func() { os.Args = saved })

	for _, tt := range tests {
		os.Args = tt.args
		if got := invokingDocker(); got != tt.want {
			t.Errorf("invokingDocker(%v) = %v, want %v", tt.args, got, tt.want)
		}
	}
}
