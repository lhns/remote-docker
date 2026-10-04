package main

import (
	"io"
	"os"
	"slices"
	"strings"
	"testing"
)

// The docker tree is built on EVERY invocation, because cobra assembles the
// whole command tree before parsing anything. So building it must not touch
// the endpoint: a probe there opens a file-serving session for commands that
// never wanted one, which makes `remote gc` race its own session and `--help`
// reach for the network.
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

// The variable is the whole reason `remote create` can write a docker context
// now that the docker LookPath finds may be us.
func TestNoSessionEnvStopsIt(t *testing.T) {
	withArgs(t, []string{"docker", "ps"})
	if !invokingDocker() {
		t.Fatal("the case being suppressed does not hold, so this proves nothing")
	}

	t.Setenv(NoSessionEnv, "1")
	if invokingDocker() {
		t.Errorf("%s did not stop a session being made available", NoSessionEnv)
	}
}

// A root flag reaches the client the command runs with. The client is
// initialised while the tree is built, before cobra parses anything, and was
// handed options nothing had filled yet: `--context bob-ws` was ignored and the
// command went wherever DOCKER_CONTEXT or the current context pointed.
// `context show` prints the context the client resolved and needs no daemon;
// DOCKER_CONTEXT names another, so only the flag winning passes.
func TestARootFlagReachesTheDockerClient(t *testing.T) {
	t.Setenv("DOCKER_CONTEXT", "alice-ws")
	t.Setenv("DOCKER_HOST", "")

	for _, args := range [][]string{
		{"--context", "bob-ws", "context", "show"},
		{"-c", "bob-ws", "context", "show"},
		{"-D", "--context=bob-ws", "context", "show"},
		{"--log-level", "error", "-cbob-ws", "context", "show"},
	} {
		var err error
		out := captureStdout(t, func() {
			withArgs(t, append([]string{"docker"}, args...))
			root := newTestRoot(t)
			root.SetArgs(args)
			err = root.Execute()
		})
		if err != nil {
			t.Fatalf("%q: %v\n%s", args, err, out)
		}
		if got := strings.TrimSpace(out); got != "bob-ws" {
			t.Errorf("%q: the client resolved context %q, want bob-ws", args, got)
		}
	}
}

// A client that cannot be initialised says why when a command runs, and the
// tree is still whole, so `--help` answers.
func TestAnInitialisationErrorSurfaces(t *testing.T) {
	conflict := []string{"--host", "tcp://127.0.0.1:1", "--context", "bob-ws"}

	run := func(args ...string) error {
		args = append(slices.Clone(conflict), args...)
		withArgs(t, append([]string{"docker"}, args...))
		root := newTestRoot(t)
		root.SetArgs(args)
		root.SetOut(io.Discard)
		root.SetErr(io.Discard)
		return root.Execute()
	}

	err := run("context", "show")
	if err == nil || !strings.Contains(err.Error(), "cannot specify both --host and --context") {
		t.Errorf("context show: got %v, want the conflict between --host and --context", err)
	}
	if err := run("ps", "--help"); err != nil {
		t.Errorf("ps --help failed beside a client that would not initialise: %v", err)
	}
}
