package main

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/lhns/remote-docker/client/internal/config"
)

func savedWorkspace(t *testing.T, name string) config.Workspace {
	t.Helper()
	file, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	return file.Workspaces[name]
}

func TestSetChangesOnlyWhatItIsGiven(t *testing.T) {
	ws := config.Workspace{
		Host: "127.0.0.1", Port: 2299, User: "alice", Watch: "partial",
		Consistency:      "read=cached",
		ConsistencyPaths: map[string]string{`C:\Users\alice\src`: "write=back"},
		IdleTimeout:      "10m",
		Machine:          &config.Machine{Backend: "wsl", Name: "rd-wsl", HostKey: "ssh-ed25519 AAAA"},
	}
	withConfig(t, &config.File{Default: "wsl", Workspaces: map[string]config.Workspace{"wsl": ws}})

	if err := run(t, "remote", "set", "wsl", "--user", "bob", "--watch", "off"); err != nil {
		t.Fatal(err)
	}
	want := ws
	want.User, want.Watch = "bob", "off"
	if got := savedWorkspace(t, "wsl"); !reflect.DeepEqual(got, want) {
		t.Errorf("saved %+v\nwant  %+v", got, want)
	}
}

func TestSetHostResetsThePortUnlessGiven(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want int
	}{
		{[]string{"--host", "wss://ws.example/tunnel"}, 0},
		{[]string{"--host", "other.example"}, config.DefaultSSHPort},
		{[]string{"--host", "other.example", "--port", "2300"}, 2300},
		{[]string{"--host", "dev.example"}, 2299},
		{[]string{"--user", "bob"}, 2299},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			withConfig(t, &config.File{Workspaces: map[string]config.Workspace{
				"dev": {Host: "dev.example", Port: 2299},
			}})
			if err := run(t, append([]string{"remote", "set", "dev"}, tc.args...)...); err != nil {
				t.Fatal(err)
			}
			if got := savedWorkspace(t, "dev").Port; got != tc.want {
				t.Errorf("port %d, want %d", got, tc.want)
			}
		})
	}
}

// A running session goes on using what it started with.
func TestSetSaysARunningSessionNeedsARestart(t *testing.T) {
	oneWorkspace(t, fakeSession(t, true, &events{}), false)

	out, err := runOut(t, "remote", "set", "dev", "--user", "bob")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "still uses the old settings\n  fix: `") || !strings.Contains(out, "remote restart dev`") {
		t.Errorf("said:\n%s", out)
	}

	// restart would look for the session at the new endpoint and miss it.
	requireFixLine(t, run(t, "remote", "set", "dev", "--endpoint", "elsewhere"), "remote stop dev")
}

// A machine row's backend suffix made WORKSPACE longer than a fixed width and
// pushed ENDPOINT right on that row alone.
func TestPrintWorkspacesAligns(t *testing.T) {
	var out bytes.Buffer
	printWorkspaces(&out, []listRow{
		{name: "*wsl", where: "alice@ssh://127.0.0.1:2222 (wsl)", endpoint: "npipe:////./pipe/docker_remote_wsl"},
		{name: " dev", where: "alice@ssh://dev.example:22", endpoint: "npipe:////./pipe/docker_remote_dev"},
		{name: " broken", err: errors.New("no host")},
	})

	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("want a header and three rows, got:\n%s", out.String())
	}
	want := strings.Index(lines[0], "ENDPOINT")
	for _, line := range lines[1:3] {
		if got := strings.Index(line, "npipe:"); got != want {
			t.Errorf("ENDPOINT starts at %d in %q, want %d as in the header:\n%s", got, line, want, out.String())
		}
	}
	if got, want := strings.Index(lines[3], "no host"), strings.Index(lines[0], "WORKSPACE"); got != want {
		t.Errorf("error starts at %d, want %d under WORKSPACE:\n%s", got, want, out.String())
	}
}
