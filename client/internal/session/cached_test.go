package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

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

// The record survives the session that wrote it, which is the whole point:
// the deletion it has to explain happened while nothing was running.
func TestCachedStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "caches", "ws.json")
	store := newCachedStore(path, nil)

	if _, ok := store.filled("/m/aaaa"); ok {
		t.Error("an empty store claimed to know a share")
	}

	store.record("/m/aaaa", []string{"/b.go", "/a.go"})
	again := newCachedStore(path, nil)

	got, ok := again.filled("/m/aaaa")
	if !ok {
		t.Fatal("the record did not survive being written and read")
	}
	// Sorted, so two runs of the same fill produce the same file rather than a
	// diff of nothing.
	if !slices.Equal(got, []string{"/a.go", "/b.go"}) {
		t.Errorf("filled = %v", got)
	}
}

// A configuration directory is a thing people sync between machines, and this
// record decides what to REMOVE from a cache. The same rule the share record
// already applies (ADR 0027).
func TestCachedStoreIgnoresARecordFromElsewhere(t *testing.T) {
	path := filepath.Join(t.TempDir(), "caches", "ws.json")
	data, err := json.Marshal(cachedFile{
		Version: cachedFileVersion,
		Machine: "some-other-laptop",
		User:    "someone-else",
		Shares:  map[string][]string{"/m/aaaa": {"/a.go"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, ok := newCachedStore(path, nil).filled("/m/aaaa"); ok {
		t.Error("a record written on another machine was believed")
	}
}
