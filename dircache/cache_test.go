package dircache

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// A share is released when nothing is bound to it (ADR 0044), and the cache
// goes with it. A session that cannot tell "no union for that" from a transient
// failure asks about it every five seconds for the rest of its life.
func TestSharesForget(t *testing.T) {
	var f shares
	f.set("/m/aaaa", "/home/me/a", &shareState{})
	f.set("/m/bbbb", "/home/me/b", &shareState{})

	f.forget("/m/aaaa")

	if _, ok := f.get("/m/aaaa"); ok {
		t.Error("a forgotten share is still tracked")
	}
	if _, ok := f.roots["/m/aaaa"]; ok {
		t.Error("a forgotten share kept its root")
	}
	if _, ok := f.manifests["/m/aaaa"]; ok {
		t.Error("a forgotten share kept its manifest")
	}
	if _, ok := f.get("/m/bbbb"); !ok {
		t.Error("forgetting one share dropped another")
	}
}

// A file deleted here while nothing was running leaves no event for anyone to
// replay, so the fill cannot carry it: a fill overwrites and adds, and has no
// way to notice what is gone. Without this the file is still in the cache, and
// still in the container, until something takes it out (ADR 0044).
func TestDeletedSince(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "kept.go"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pkg", "also.go"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := deletedSince(root, []string{"/kept.go", "/gone.go", "/pkg/also.go", "/pkg/vanished.go"})
	want := []string{"/gone.go", "/pkg/vanished.go"}
	if !slices.Equal(got, want) {
		t.Errorf("deletedSince = %v, want %v", got, want)
	}
}
