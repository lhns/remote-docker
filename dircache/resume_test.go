package dircache

import (
	"os"
	"path/filepath"
	"testing"
)

// A fill that resumes after a restart sends only what the cache does not hold.
// Sending a file the record says went, and that has not changed here since,
// would overwrite what a container running through the restart wrote over it.
func TestAResumedFillDoesNotResendWhatTheCacheHolds(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"held.go", "edited.go", "new.go"} {
		if err := os.WriteFile(filepath.Join(root, n), []byte("package x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	held, _ := baselineOf(root, "held.go")
	edited, _ := baselineOf(root, "edited.go")
	edited.Size++ // as it was sent, before it changed here

	rec := &fakeRecord{filled: map[string]map[string]Baseline{wbShare: {
		"/held.go":   held,
		"/edited.go": edited,
	}}}
	store := &faultStore{}
	c := &Cache{Store: func() (Store, bool) { return store, true }, Record: rec, Ctx: t.Context()}
	t.Cleanup(c.Stop)

	c.Attach(wbShare, root, ShareOptions{Policy: PolicyEager})
	eventually(t, "the resumed fill to finish", func() bool {
		r := c.Reports()
		return len(r) == 1 && r[0].Done && r[0].Stats.Complete()
	})

	store.mu.Lock()
	defer store.mu.Unlock()
	var sent []string
	for _, e := range store.applied {
		sent = append(sent, e.Path)
	}
	want := map[string]bool{"edited.go": true, "new.go": true}
	if len(sent) != 2 || !want[sent[0]] || !want[sent[1]] {
		t.Errorf("sent %v, want only edited.go (changed here) and new.go (never sent)", sent)
	}
}
