package main

import (
	"os"
	"testing"
)

// The docker tree is built on EVERY invocation, because cobra assembles the
// whole command tree before parsing anything. It used to probe the endpoint
// and could open a file-serving session while building, so `remote gc` raced
// its own session, and `--help` reached for the network.
func TestInvokingDocker(t *testing.T) {
	tests := []struct {
		args []string
		want bool
	}{
		{[]string{"docker", "ps"}, true},
		{[]string{"docker", "run", "--rm", "alpine"}, true},
		{[]string{"docker", "--context", "dev", "ps"}, true},

		// Ours. `remote` and everything under it resolves its own session, on
		// its own workspace, and must not have one opened for it here.
		{[]string{"docker", "remote", "gc"}, false},
		{[]string{"docker", "remote", "status"}, false},
		{[]string{"docker", "remote", "--workspace", "ps", "status"}, false},

		// No subcommand is no daemon. `docker`, `--help` and `--version` all
		// print something local, and printing it must not open an SSH
		// connection, an NFS server and a reverse tunnel.
		{[]string{"docker"}, false},
		{[]string{"docker", "--help"}, false},
		{[]string{"docker", "--version"}, false},

		// A flag's value is not the subcommand.
		{[]string{"docker", "--context", "remote", "ps"}, true},
		{[]string{"docker", "--log-level", "debug", "ps"}, true},

		// The commands that reach no daemon. `context` is the one that
		// matters: this binary may BE the `docker` on PATH, so `remote create`
		// writing a context spawns US, and a session to write a line of JSON
		// is absurd.
		{[]string{"docker", "context", "ls"}, false},
		{[]string{"docker", "context", "create", "dev"}, false},
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
