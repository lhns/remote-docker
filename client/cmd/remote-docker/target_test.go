package main

// What a docker invocation is aimed at.
//
// The rule this exists for is the one where the answer is "nothing": a context
// somebody else created is an instruction to talk to somebody else, and we
// used to override it by setting DOCKER_HOST, which outranks a context in
// docker's own resolution. There is no way to observe that from outside the
// process, so the decision is a pure function and this is a table over it.

import (
	"testing"
)

// fakeLookups answers from fixed data. `ours` is the set of contexts this
// program created; everything else is somebody else's.
func fakeLookups(env map[string]string, current string, ours map[string]string) lookups {
	return lookups{
		getenv:         func(k string) string { return env[k] },
		currentContext: func() string { return current },
		contextIsOurs:  func(name string) bool { _, ok := ours[name]; return ok },
		workspaceFor:   func(name string) string { return ours[name] },
		endpointIsOurs: func(host string) bool { return host == "npipe:////./pipe/docker_remote_dev" },
	}
}

func TestDecideTarget(t *testing.T) {
	ours := map[string]string{"dev": "dev", "ci": "ci"}

	for _, tc := range []struct {
		name    string
		args    []string
		env     map[string]string
		current string
		want    target
	}{
		// Nothing is selected anywhere, so nothing is being overridden.
		{
			name: "no context and no host",
			args: []string{"ps"},
			want: target{ensure: true, setHost: true},
		},

		// Ours, named on the command line. The session is for THAT workspace,
		// and DOCKER_HOST stays out of it so docker reads the context.
		{
			name: "our context by flag",
			args: []string{"--context", "ci", "ps"},
			want: target{workspace: "ci", ensure: true},
		},
		{
			name: "our context by flag, joined",
			args: []string{"--context=ci", "ps"},
			want: target{workspace: "ci", ensure: true},
		},
		{
			name: "our context by shorthand",
			args: []string{"-c", "ci", "ps"},
			want: target{workspace: "ci", ensure: true},
		},
		{
			name: "our context from the environment",
			args: []string{"ps"},
			env:  map[string]string{"DOCKER_CONTEXT": "dev"},
			want: target{workspace: "dev", ensure: true},
		},
		{
			name:    "our context as the current one",
			args:    []string{"ps"},
			current: "dev",
			want:    target{workspace: "dev", ensure: true},
		},

		// Somebody else's, by every route. This is the bug: all of these
		// reached us instead of the daemon they name.
		{
			name: "a foreign context by flag",
			args: []string{"--context", "desktop", "ps"},
			want: leaveAlone,
		},
		{
			name: "a foreign context from the environment",
			args: []string{"ps"},
			env:  map[string]string{"DOCKER_CONTEXT": "desktop"},
			want: leaveAlone,
		},
		{
			name:    "a foreign context as the current one",
			args:    []string{"ps"},
			current: "desktop",
			want:    leaveAlone,
		},

		// An explicit host outranks a context in docker's resolution, so it
		// outranks one here.
		{
			name: "an explicit --host",
			args: []string{"--host", "tcp://10.0.0.1:2375", "ps"},
			want: leaveAlone,
		},
		{
			name: "an explicit -H, even with our context",
			args: []string{"-H", "tcp://10.0.0.1:2375", "--context", "dev", "ps"},
			want: leaveAlone,
		},
		{
			name: "DOCKER_HOST naming somebody else",
			args: []string{"ps"},
			env:  map[string]string{"DOCKER_HOST": "tcp://10.0.0.1:2375"},
			want: leaveAlone,
		},

		// DOCKER_HOST naming OUR endpoint is not a foreign instruction: it is
		// what our own printed value and our own context both say. Treating it
		// as foreign stopped us starting a session or noticing a stale one.
		{
			name: "DOCKER_HOST naming ours",
			args: []string{"ps"},
			env:  map[string]string{"DOCKER_HOST": "npipe:////./pipe/docker_remote_dev"},
			want: target{ensure: true},
		},

		// A flag's value is not a context name, and a container flag that
		// happens to share a letter is not the root's.
		{
			name: "a -c after the subcommand belongs to the subcommand",
			args: []string{"run", "-c", "512", "alpine"},
			want: target{ensure: true, setHost: true},
		},
		{
			name: "another flag's value is skipped",
			args: []string{"--log-level", "debug", "--context", "ci", "ps"},
			want: target{workspace: "ci", ensure: true},
		},
		{
			name: "a context named like a flag's value",
			args: []string{"--config", "ci", "ps"},
			want: target{ensure: true, setHost: true},
		},

		// Asked for and left empty. Docker will reject it; what matters is
		// that we do not go picking a default behind it.
		{
			name: "--context with nothing after it",
			args: []string{"--context"},
			want: leaveAlone,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := decideTarget(tc.args, fakeLookups(tc.env, tc.current, ours))
			if got != tc.want {
				t.Errorf("decideTarget(%v) = %+v, want %+v", tc.args, got, tc.want)
			}
		})
	}
}

// The two halves that matter, said plainly, because a table row is easy to
// read past.
func TestAForeignContextIsNeverRedirected(t *testing.T) {
	got := decideTarget([]string{"--context", "desktop", "ps"},
		fakeLookups(nil, "", map[string]string{"dev": "dev"}))

	if got.ensure {
		t.Error("a session was opened for a context we did not create")
	}
	if got.setHost {
		t.Error("DOCKER_HOST would have been set, overriding the context")
	}
}

func TestOurContextIsNotOverriddenByDockerHost(t *testing.T) {
	got := decideTarget([]string{"--context", "ci", "ps"},
		fakeLookups(nil, "", map[string]string{"ci": "ci"}))

	if got.workspace != "ci" {
		t.Errorf("session opened for %q, want ci", got.workspace)
	}
	// The whole point: DOCKER_HOST outranks --context, so setting it would
	// send the command to the default workspace instead of the named one.
	if got.setHost {
		t.Error("DOCKER_HOST would have been set, overriding the context we honoured")
	}
}
