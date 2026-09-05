package dircache

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// writeTree writes files of the given sizes and returns the root.
func writeTree(t *testing.T, files map[string]int) string {
	t.Helper()
	root := t.TempDir()
	for name, size := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func paths(entries []Entry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Path)
	}
	return out
}

// The cache may hold only what the watcher can invalidate. An excluded
// directory is served live instead, which is slower and right.
func TestWalkSkipsWhatTheWatcherDoesNot(t *testing.T) {
	root := writeTree(t, map[string]int{
		"main.go":             10,
		".git/objects/aaaa":   10,
		"node_modules/x/y.js": 10,
	})

	var found []Entry
	stats := walk(root, []string{".git", "node_modules"}, func(e Entry) { found = append(found, e) })

	if got := paths(found); len(got) != 1 || got[0] != "main.go" {
		t.Errorf("walked %v, want only main.go", got)
	}
	if stats.Excluded != 2 {
		t.Errorf("Excluded = %d, want the two directories", stats.Excluded)
	}
}

// A directory holds no bytes of its own and travels with the files inside it;
// what a tar cannot carry to another machine is not cached at all.
func TestWalkYieldsOnlyRegularFiles(t *testing.T) {
	root := writeTree(t, map[string]int{"pkg/deep/file.go": 8})
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	var found []Entry
	walk(root, nil, func(e Entry) { found = append(found, e) })
	if got := paths(found); len(got) != 1 || got[0] != "pkg/deep/file.go" {
		t.Errorf("walked %v, want the one file", got)
	}
}

// An invalidation's paths arrive all at once rather than through the tree, so
// they need the same bound a prefetch's sends have: one tar held whole in
// memory, and lost entirely if anything about it fails.
func TestBatchesBoundsByBytesAndByCount(t *testing.T) {
	var big []Entry
	for i := range 5 {
		big = append(big, Entry{Path: fmt.Sprintf("big%d.bin", i), Size: batchBytes / 2})
	}
	if got := len(batches(big)); got < 3 {
		t.Errorf("%d entries of half a batch each went out in %d batches", len(big), got)
	}

	var many []Entry
	for i := range maxBatchFiles * 2 {
		many = append(many, Entry{Path: fmt.Sprintf("f%05d.go", i)})
	}
	batches := batches(many)
	if len(batches) != 2 {
		t.Errorf("%d empty files went out in %d batches, want 2", len(many), len(batches))
	}
	for _, b := range batches {
		if len(b) > maxBatchFiles {
			t.Errorf("a batch holds %d files, over the %d cap", len(b), maxBatchFiles)
		}
	}
}

// One entry larger than a whole batch still goes, alone: refusing it would drop
// a file from the cache for being big, and the fill's own budget is what
// decides whether a big file is worth caching at all.
func TestBatchesKeepsAnOversizedEntry(t *testing.T) {
	batches := batches([]Entry{{Path: "huge.bin", Size: batchBytes * 3}, {Path: "a.go", Size: 4}})
	if len(batches) != 2 || len(batches[0]) != 1 {
		t.Errorf("an oversized entry was batched wrongly: %d batches", len(batches))
	}
}

// Nothing to send is no batches, rather than one empty one.
func TestBatchesOfNothing(t *testing.T) {
	if got := batches(nil); len(got) != 0 {
		t.Errorf("batches(nil) = %v, want none", got)
	}
}
