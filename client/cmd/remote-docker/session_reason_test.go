package main

// Why a docker command could not reach a session.
//
// Nothing serving the endpoint makes the embedded CLI report a missing daemon:
//
//	failed to connect to the docker API at npipe:////./pipe/docker_remote ...
//	The system cannot find the file specified.
//
// True, and useless, because the daemon is missing for a reason this program
// knows and that message does not carry. These pin that the reason is returned
// rather than swallowed.

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/lhns/remote-docker/client/internal/config"
	"github.com/lhns/remote-docker/client/internal/proxy"
)

// unreachableEndpoint is an address nothing is serving, so ensureDaemon takes
// the branch that decides whether a session can be started at all.
func unreachableEndpoint(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		return `\\.\pipe\remote-docker-nothing-here-` + t.Name()
	}
	return filepath.Join(t.TempDir(), "nothing-here.sock")
}

// No workspace configured is the first thing somebody hits, and the fix is a
// setting they have to be told about.
//
// It must not try to start anything either: there is nowhere to connect to, so
// spawning a session would only produce a second failure twenty seconds later.
func TestEnsureDaemonSaysWhenNoWorkspaceIsConfigured(t *testing.T) {
	err := ensureDaemon(config.Config{}, unreachableEndpoint(t))
	if err == nil {
		t.Fatal("a docker command with no workspace configured got no error, " +
			"so the CLI would report a missing daemon instead")
	}

	// The same message `remote status` gives, because the situation is the same
	// one and two spellings of it would drift.
	want := config.Config{}.RequireHost().Error()
	if err.Error() != want {
		t.Errorf("said %q,\nwant %q", err, want)
	}
	// The remedy, which is the part that makes it actionable.
	if !strings.Contains(err.Error(), config.EnvHost) {
		t.Errorf("the error does not name the setting that fixes it: %v", err)
	}
}

// A session that IS serving is not an error, whatever else is true of it: the
// command about to run will work.
//
// Serving but not answering our control channel is the awkward case -- an older
// session, or something that is not ours at all. Left alone, and silently,
// because taking over what we cannot identify is worse than the mismatch.
func TestEnsureDaemonIsQuietWhenSomethingIsServing(t *testing.T) {
	endpoint := unreachableEndpoint(t)
	l, err := proxy.Listen(endpoint)
	if err != nil {
		t.Skipf("cannot bind a test endpoint here: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	// Accept and say nothing, which is what "serving but not answering" is.
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	if err := ensureDaemon(config.Config{Host: "workspace.invalid"}, endpoint); err != nil {
		t.Errorf("a served endpoint reported %v, want no error", err)
	}
}
