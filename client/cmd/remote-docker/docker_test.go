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

		// Under the alias, every argument is already docker's.
		{[]string{"docker", "ps"}, true},
		{[]string{`C:\bin\docker.exe`, "run", "--rm", "alpine"}, true},
		{[]string{"docker", "--context", "dev", "ps"}, true},

		// No subcommand is no daemon, under either name. `docker` alone,
		// `docker --help` and `remote-docker docker` all print help, and
		// printing help must not open an SSH connection, an NFS server and a
		// reverse tunnel -- which is the same reason the comment above exists.
		{[]string{"docker"}, false},
		{[]string{"docker", "--help"}, false},
		{[]string{"docker", "--version"}, false},
		{[]string{"remote-docker", "docker"}, false},

		// The commands that reach no daemon. `context` is the one that
		// matters: once `docker` on PATH is this binary, `workspace create`
		// writing a context spawns US, and a session to write a line of JSON
		// is absurd.
		{[]string{"docker", "context", "ls"}, false},
		{[]string{"remote-docker", "docker", "context", "create", "dev"}, false},
		{[]string{"docker", "completion", "bash"}, false},
		{[]string{"docker", "help"}, false},
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
