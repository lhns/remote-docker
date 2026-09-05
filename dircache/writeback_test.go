package dircache

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// cacheWith returns a Cache wired to the store, with no logging.
func cacheWith(t *testing.T, store *fakeStore) *Cache {
	t.Helper()
	c := &Cache{
		Store:  func() (Store, bool) { return store, true },
		Ctx:    t.Context(),
		Policy: PolicyTree,
	}
	t.Cleanup(c.Stop)
	return c
}

// The store names the paths that come back, and a store is not this machine's
// to trust with one: a tar entry called ../../.ssh/authorized_keys would
// otherwise be written wherever it pointed.
func TestWriteUnderRefusesAPathThatLeavesTheShare(t *testing.T) {
	root := t.TempDir()

	for _, name := range []string{
		"/../escaped",
		"/../../etc/passwd",
		"/a/../../escaped",
		"/a/b/../../../escaped",
	} {
		err := writeUnder(root, File{Path: name, Mode: 0o644, Body: strings.NewReader("x")})
		if !errors.Is(err, errEscapes) {
			t.Errorf("writeUnder(%q) = %v, want a refusal", name, err)
		}
	}

	// Nothing may have been created on the way to refusing, including in the
	// parent, which is where a "../" lands.
	for _, dir := range []string{root, filepath.Dir(root)} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("reading %s: %v", dir, err)
		}
		for _, e := range entries {
			if strings.Contains(e.Name(), "escaped") {
				t.Errorf("a refused path still created %s in %s", e.Name(), dir)
			}
		}
	}
}

// The share root is a prefix of its own siblings, so a check on the string
// alone lets /home/alice/project-old through for a share at /home/alice/project.
func TestWriteUnderRefusesASiblingOfTheShare(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "project")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}

	err := writeUnder(root, File{Path: "/../project-old/x", Mode: 0o644, Body: strings.NewReader("x")})
	if !errors.Is(err, errEscapes) {
		t.Fatalf("writeUnder into a sibling = %v, want a refusal", err)
	}
}

// What comes back keeps the time the consumer wrote it, which is what a plain
// mount would have shown and what the next round compares against. A file
// written with this machine's clock instead looks changed here forever.
func TestWriteUnderKeepsTheModificationTime(t *testing.T) {
	root := t.TempDir()
	wrote := time.Now().Add(-72 * time.Hour).Truncate(time.Second)

	err := writeUnder(root, File{
		Path: "/pkg/deep/lib.go", ModTime: wrote, Mode: 0o644,
		Body: strings.NewReader("package deep\n"),
	})
	if err != nil {
		t.Fatalf("writeUnder: %v", err)
	}

	// The directories were made on the way, which is the only reason a nested
	// path works at all.
	info, err := os.Stat(filepath.Join(root, "pkg", "deep", "lib.go"))
	if err != nil {
		t.Fatalf("the file was not written: %v", err)
	}
	if !info.ModTime().Truncate(time.Second).Equal(wrote) {
		t.Errorf("mtime = %v, want %v", info.ModTime(), wrote)
	}
}

// One round, from a file the fill sent to the same file changed in the cache
// and landing back on this machine.
func TestWriteBackShareAppliesAChange(t *testing.T) {
	root := t.TempDir()
	local := filepath.Join(root, "main.go")
	if err := os.WriteFile(local, []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(local)
	if err != nil {
		t.Fatal(err)
	}

	changedAt := info.ModTime().Add(time.Minute)
	store := &fakeStore{
		changes: []Change{
			{Path: "/main.go", Size: 6, ModTime: changedAt.UnixNano()},
		},
		files: map[string]File{
			"/main.go": {Path: "/main.go", ModTime: changedAt, Mode: 0o644, Body: strings.NewReader("after\n")},
		},
	}

	c := cacheWith(t, store)
	c.shares.set("/cwd", root, &shareState{Report: Report{Done: true}, Cached: true})
	c.shares.noteSent("/cwd", root, []Entry{{Path: "main.go", Size: 7}})

	c.writeBackShare(t.Context(), "/cwd")

	got, err := os.ReadFile(local)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "after\n" {
		t.Errorf("the file holds %q, want the consumer's version", got)
	}

	// And the manifest moved with it, or the next round decides the same
	// change again and every round after that reports a conflict with itself.
	base, ok := c.shares.baselines("/cwd")["/main.go"]
	if !ok {
		t.Fatal("the written-back file left the manifest")
	}
	if base.Size != 6 {
		t.Errorf("the baseline is %d bytes, want what was just written", base.Size)
	}
}

// A share released because nothing is bound to it any more is a reason to stop
// asking, not to retry: the cache went with it, so there is nothing to carry
// back and nothing to compare against.
func TestWriteBackShareStopsWhenTheShareIsGone(t *testing.T) {
	store := &fakeStore{changesErr: ErrShareGone}
	c := cacheWith(t, store)
	c.shares.set("/cwd", t.TempDir(), &shareState{Report: Report{Done: true}, Cached: true})

	c.writeBackShare(t.Context(), "/cwd")

	if _, ok := c.shares.get("/cwd"); ok {
		t.Error("a released share is still polled")
	}
}

// Nothing is asked for while the fill is still running: there is no settled
// baseline to compare against, and a half-filled cache cannot say whether a
// missing file was never sent or was deleted.
func TestWriteBackShareWaitsForTheFill(t *testing.T) {
	store := &fakeStore{
		changes: []Change{{Path: "/main.go", Size: 1, ModTime: 1}},
	}
	c := cacheWith(t, store)
	c.shares.set("/cwd", t.TempDir(), &shareState{})

	c.writeBackShare(t.Context(), "/cwd")

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.pulled) != 0 {
		t.Errorf("pulled %v while the fill was still running", store.pulled)
	}
}

// Only what a fill recorded and this machine no longer has. A path in the cache
// that no fill sent is the consumer's own file, and removing one would destroy
// a build's output on every reconnect.
func TestDropDeletedAsksOnlyForWhatIsGone(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "kept.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := &fakeStore{}
	c := cacheWith(t, store)
	c.dropDeleted("/cwd", root, []string{"/kept.go", "/gone.go"})

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.dropped) != 1 || store.dropped[0] != "/gone.go" {
		t.Errorf("dropped %v, want only /gone.go", store.dropped)
	}
}

// With nothing missing there is no exchange at all. This runs on every fill and
// on every dropped-event reconcile, so a round trip to say nothing is a round
// trip per share per reconnect.
func TestDropDeletedSaysNothingWhenNothingIsGone(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := &fakeStore{}
	c := cacheWith(t, store)
	c.dropDeleted("/cwd", root, []string{"/a.go"})

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.dropped) != 0 {
		t.Errorf("dropped %v with nothing missing", store.dropped)
	}
}
