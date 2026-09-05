package dircache

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A share attached without prefetch holds only what the consumer wrote, so
// its cache is complete the moment it is attached and write-back must carry
// a file the consumer created. Left "incomplete", a read=direct,write=back
// share never writes anything back.
func TestAttachWithoutPrefetchWritesBack(t *testing.T) {
	root := t.TempDir()
	writtenAt := time.Now().Add(-time.Minute)
	store := &fakeStore{
		changes: []Change{
			{Path: "/out.txt", Size: 6, ModTime: writtenAt.UnixNano()},
		},
		files: map[string]File{
			"/out.txt": {Path: "/out.txt", ModTime: writtenAt, Mode: 0o644, Body: strings.NewReader("there\n")},
		},
	}
	c := cacheWith(t, store)
	c.Attach("/cwd", root, ShareOptions{Prefetch: false})

	c.writeBackShare(t.Context(), "/cwd")

	got, err := os.ReadFile(filepath.Join(root, "out.txt"))
	if err != nil {
		t.Fatalf("the consumer's file was not written back: %v", err)
	}
	if string(got) != "there\n" {
		t.Errorf("the file holds %q, want the consumer's version", got)
	}
}

// A share that prefetches nothing, whether because it was attached without
// prefetch or because the policy is off, is still known for invalidation and
// write-back, reports done, and sends nothing, ever.
func TestAttachWithNothingToPrefetch(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy Policy
		opts   ShareOptions
	}{
		{"attached without prefetch", PolicyTree, ShareOptions{Prefetch: false}},
		{"policy off", PolicyOff, ShareOptions{Prefetch: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{}
			c, root := treeCache(t, store)
			c.Policy = tc.policy
			c.Attach("/cwd", root, tc.opts)

			c.Touch("/cwd", "/pkga/faa.go", 2000)
			time.Sleep(50 * time.Millisecond)
			if n := store.appliedCount(); n != 0 {
				t.Errorf("%d files were sent", n)
			}
			reports := c.Reports()
			if len(reports) != 1 || !reports[0].Done {
				t.Errorf("reports = %+v, want the share known and done", reports)
			}
		})
	}
}

// Once the walk is over and the tree has sent everything the budget allows,
// the status reports the share as cached: the files the tree holds ARE what
// will be cached, against the walk's total.
func TestTreeFillReportsComplete(t *testing.T) {
	store := &fakeStore{}
	c, root := treeCache(t, store)
	c.Attach("/cwd", root, ShareOptions{Prefetch: true})

	eventually(t, "the fill to finish", func() bool {
		r := c.Reports()
		return len(r) == 1 && r[0].Done
	})
	r := c.Reports()[0]
	if !r.Stats.Complete() {
		t.Errorf("stats = %+v, want every file the walk saw to be cached", r.Stats)
	}
	if r.Stats.TotalFiles != 120 || r.Sent != 120 {
		t.Errorf("total %d sent %d, want 120 of 120", r.Stats.TotalFiles, r.Sent)
	}
	if r.Stats.Bytes == 0 || r.Stats.Bytes != r.Stats.TotalBytes {
		t.Errorf("bytes %d of %d, want the tree's whole size", r.Stats.Bytes, r.Stats.TotalBytes)
	}
}

// A prefetch runs once per share. A second consumer of the same directory
// finding it already running is the ordinary case, and re-sending the tree
// would cost the link twice for nothing.
func TestAttachRunsOncePerShare(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.go", "b.go"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("package x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	store := &fakeStore{}
	c := cacheWith(t, store)

	c.Attach("/cwd", root, ShareOptions{Prefetch: true})
	eventually(t, "the prefetch to finish", func() bool { r := c.Reports(); return len(r) == 1 && r[0].Done })

	c.Attach("/cwd", root, ShareOptions{Prefetch: true})
	eventually(t, "one report", func() bool { return len(c.Reports()) == 1 })

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.applied) != 2 {
		t.Errorf("applied %d entries over two attaches, want the tree once", len(store.applied))
	}
}

// A prefetch with nowhere to send its batches is not an error: the share is
// served from the authoritative tree meanwhile, and the next one starts from
// scratch.
func TestAttachWithNoStore(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := &Cache{Store: func() (Store, bool) { return nil, false }, Ctx: t.Context(), Policy: PolicyTree}
	t.Cleanup(c.Stop)

	c.Attach("/cwd", root, ShareOptions{Prefetch: true})
	eventually(t, "the prefetch to finish", func() bool { r := c.Reports(); return len(r) == 1 && r[0].Done })

	if err := c.Reports()[0].Err; err != nil {
		t.Errorf("a prefetch with no store reported %v, want it to pass quietly", err)
	}
}
