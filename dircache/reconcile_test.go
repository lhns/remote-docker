package dircache

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

type fakeRecord struct {
	mu     sync.Mutex
	filled map[string][]string
}

func (r *fakeRecord) Filled(share string) ([]string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	paths, ok := r.filled[share]
	return paths, ok
}

func (r *fakeRecord) Record(share string, paths []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.filled == nil {
		r.filled = map[string][]string{}
	}
	r.filled[share] = paths
}

// A file deleted here while nothing ran is taken out of the cache on the next
// attach whatever the prefetch policy: the record of an earlier fill is the
// only thing that can, and a cache filled last session is still serving it.
func TestAttachReconcilesDeletionsWithPrefetchOff(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "kept.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &fakeStore{}
	c := cacheWith(t, store)
	c.Policy = PolicyOff
	c.Record = &fakeRecord{filled: map[string][]string{"/cwd": {"kept.go", "gone.go"}}}

	c.Attach("/cwd", root, ShareOptions{Prefetch: true})

	eventually(t, "the deleted file to be dropped", func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return len(store.dropped) == 1 && store.dropped[0] == "/gone.go"
	})
}

// deletedWhileAway runs the next session: the file is deleted here, and the
// record of the session before is all that can take it out of the cache.
func deletedWhileAway(t *testing.T, record Record, root, file string) {
	t.Helper()
	if err := os.Remove(filepath.Join(root, file)); err != nil {
		t.Fatal(err)
	}
	store := &fakeStore{}
	c := cacheWith(t, store)
	c.Policy = PolicyOff
	c.Record = record
	c.Attach("/cwd", root, ShareOptions{})

	eventually(t, "/"+file+" to be dropped from the cache", func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return slices.Contains(store.dropped, "/"+file)
	})
}

// A file created here reaches the cache through invalidation, not the fill,
// and has to be recorded all the same.
func TestAnInvalidationBatchIsRecorded(t *testing.T) {
	root := t.TempDir()
	record := &fakeRecord{}
	store := &fakeStore{}
	c := cacheWith(t, store)
	c.Policy = PolicyOff
	c.Record = record
	c.Attach("/cwd", root, ShareOptions{})

	if err := os.WriteFile(filepath.Join(root, "new.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	c.Observe(Event{Share: "/cwd", Path: "/new.go", Op: OpCreate})
	eventually(t, "the new file to reach the cache", func() bool { return store.appliedCount() == 1 })

	deletedWhileAway(t, record, root, "new.go")
}

// A prefetch cut off midway may have landed part of its batch, so what it sent
// is recorded before it is applied.
func TestAnInterruptedPrefetchIsRecorded(t *testing.T) {
	record := &fakeRecord{}
	store := &fakeStore{applyErr: errors.New("the connection went")}
	c, root := treeCache(t, store)
	c.Policy = PolicyEager
	c.Record = record
	c.Attach("/cwd", root, ShareOptions{Prefetch: true})
	eventually(t, "the prefetch to fail", func() bool {
		r := c.Reports()
		return len(r) == 1 && r[0].Done && r[0].Err != nil
	})

	deletedWhileAway(t, record, root, "pkga/faa.go")
}

// An earlier session's copy of a file since deleted here is not the consumer's
// new file: write-back leaves it, and a file the consumer did create still
// comes back.
func TestWriteBackRefusesARecordedFileDeletedHere(t *testing.T) {
	root := t.TempDir()
	store := &fakeStore{
		changes: []Change{
			{Path: "/stale.go", Size: 1, ModTime: 1},
			{Path: "/created.go", Size: 1, ModTime: 1},
		},
		files: map[string]File{
			"/stale.go":   {Path: "/stale.go", Body: strings.NewReader("x")},
			"/created.go": {Path: "/created.go", Body: strings.NewReader("x")},
		},
	}
	c := cacheWith(t, store)
	c.Policy = PolicyOff
	c.Record = &fakeRecord{filled: map[string][]string{"/cwd": {"/stale.go"}}}
	c.Attach("/cwd", root, ShareOptions{})

	c.writeBackShare(t.Context(), "/cwd")

	if _, err := os.Stat(filepath.Join(root, "stale.go")); err == nil {
		t.Error("a file deleted here was brought back from the cache")
	}
	if _, err := os.Stat(filepath.Join(root, "created.go")); err != nil {
		t.Errorf("the consumer's new file did not come back: %v", err)
	}
}
