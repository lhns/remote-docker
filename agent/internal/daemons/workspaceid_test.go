package daemons

import (
	"os"
	"path/filepath"
	"testing"
)

// The id must be the same on the next run, because that is the entire reason
// it exists rather than using the container id.
func TestTheWorkspaceIDSurvivesARestart(t *testing.T) {
	dir := t.TempDir()

	first, err := WorkspaceID(dir)
	if err != nil {
		t.Fatalf("WorkspaceID: %v", err)
	}
	if first == "" {
		t.Fatal("no id was generated")
	}

	second, err := WorkspaceID(dir)
	if err != nil {
		t.Fatalf("WorkspaceID again: %v", err)
	}
	if second != first {
		t.Errorf("the id changed across runs: %q then %q; every daemon would be orphaned", first, second)
	}
}

// Two workspaces sharing a parent daemon must not adopt each other's daemons.
func TestTwoWorkspacesGetDifferentIDs(t *testing.T) {
	a, err := WorkspaceID(t.TempDir())
	if err != nil {
		t.Fatalf("WorkspaceID: %v", err)
	}
	b, err := WorkspaceID(t.TempDir())
	if err != nil {
		t.Fatalf("WorkspaceID: %v", err)
	}
	if a == b {
		t.Errorf("two workspaces got the same id %q; each would adopt the other's daemons", a)
	}
}

// A half-finished first run leaves an empty file. Reading it back as the id
// would label every daemon with the empty string, which matches every OTHER
// workspace that failed the same way.
func TestAnEmptyIDFileIsReplaced(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, WorkspaceIDFile)
	if err := os.WriteFile(path, []byte("  \n"), 0o600); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	id, err := WorkspaceID(dir)
	if err != nil {
		t.Fatalf("WorkspaceID: %v", err)
	}
	if id == "" {
		t.Fatal("an empty file was read back as the id")
	}

	// And it was written, so the next run agrees.
	again, err := WorkspaceID(dir)
	if err != nil {
		t.Fatalf("WorkspaceID again: %v", err)
	}
	if again != id {
		t.Errorf("the replacement was not persisted: %q then %q", id, again)
	}
}

// Whitespace around a hand-edited id must not make it a different id.
func TestTheIDIsTrimmed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, WorkspaceIDFile), []byte("ws-abc123\n"), 0o600); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	id, err := WorkspaceID(dir)
	if err != nil {
		t.Fatalf("WorkspaceID: %v", err)
	}
	if id != "ws-abc123" {
		t.Errorf("id = %q, want it trimmed", id)
	}
}
