package dircache

import (
	"os"
	"path/filepath"
	"testing"
)

type fakeRecord struct{ filled map[string][]string }

func (r *fakeRecord) Filled(share string) ([]string, bool) {
	paths, ok := r.filled[share]
	return paths, ok
}

func (r *fakeRecord) Record(share string, paths []string) {
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
