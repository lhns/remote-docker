package main

// Commands that meet the same situation answer it the same way: one line of
// diagnosis and one `fix:` line.

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/lhns/remote-docker/client/internal/config"
)

// withConfig points the config at a temporary home holding file.
func withConfig(t *testing.T, file *config.File) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, env := range []string{config.EnvHost, config.EnvWorkspace, config.EnvEndpoint} {
		t.Setenv(env, "")
	}
	if file != nil {
		if err := config.Save(*file, ""); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}
}

// runOut is run with stdout kept.
func runOut(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newTestRoot(t)
	var out bytes.Buffer
	root.SetArgs(args)
	root.SetOut(&out)
	root.SetErr(&out)
	err := root.Execute()
	return out.String(), err
}

func requireFixLine(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatal("no error")
	}
	lines := strings.Split(err.Error(), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[1], "  fix: ") {
		t.Fatalf("want one line of diagnosis and one fix line, got %q", err)
	}
	if !strings.Contains(lines[1], want) {
		t.Errorf("fix line %q does not name %q", lines[1], want)
	}
}

func TestAnUnknownWorkspaceNameIsAnsweredOnce(t *testing.T) {
	withConfig(t, &config.File{
		Default:    "main",
		Workspaces: map[string]config.Workspace{"main": {Host: "main.example"}},
	})
	want := noWorkspaceNamed("nope").Error()

	for _, args := range [][]string{
		{"remote", "use", "nope"},
		{"remote", "rm", "nope"},
		{"remote", "machine", "status", "nope"},
		{"remote", "machine", "stop", "nope"},
		{"remote", "--workspace", "nope", "status"},
		{"remote", "--workspace", "nope", "machine", "status"},
		{"remote", "stop", "nope"},
		{"remote", "inspect", "nope"},
	} {
		t.Run(strings.Join(args[1:], " "), func(t *testing.T) {
			err := run(t, args...)
			if err == nil || err.Error() != want {
				t.Errorf("said %v, want %q", err, want)
			}
		})
	}
	requireFixLine(t, noWorkspaceNamed("nope"), "remote ls")
}

func TestNoWorkspaceConfiguredNamesTheRemedy(t *testing.T) {
	withConfig(t, nil)

	for _, args := range [][]string{
		{"remote", "status"},
		{"remote", "start"},
		{"remote", "restart"},
		{"remote", "gc"},
		{"remote", "inspect"},
	} {
		t.Run(strings.Join(args[1:], " "), func(t *testing.T) {
			err := run(t, args...)
			if !errors.Is(err, config.ErrNoWorkspace) {
				t.Fatalf("said %v, want ErrNoWorkspace", err)
			}
			requireFixLine(t, err, "remote create")
		})
	}
}

func TestCreateWithoutAHostNamesTheRemedy(t *testing.T) {
	withConfig(t, nil)
	requireFixLine(t, run(t, "remote", "create", "dev"), "remote create dev --host")
}

// inspect with no name answers for --workspace, as every other command does,
// rather than for the default.
func TestInspectHonoursWorkspaceFlag(t *testing.T) {
	withConfig(t, &config.File{
		Default: "main",
		Workspaces: map[string]config.Workspace{
			"main":  {Host: "main.example"},
			"other": {Host: "other.example"},
		},
	})

	out, err := runOut(t, "remote", "--workspace", "other", "inspect")
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if !strings.Contains(out, "other.example") {
		t.Errorf("inspect --workspace other showed:\n%s", out)
	}

	// The positional name still wins.
	out, err = runOut(t, "remote", "--workspace", "other", "inspect", "main")
	if err != nil {
		t.Fatalf("inspect main: %v", err)
	}
	if !strings.Contains(out, "main.example") {
		t.Errorf("inspect main showed:\n%s", out)
	}
}
