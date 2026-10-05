package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

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
