package dircache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lhns/remote-docker/core/cache"
)

// fakeStore is a Store that holds what it was given, so the policy can be
// driven end to end without a workspace, a connection or a tar.
type fakeStore struct {
	mu sync.Mutex

	applied []Entry
	dropped []string
	pulled  []string

	changes    []cache.Change
	changesErr error

	// files is what Pull hands back, by share-relative path.
	files map[string]File
}

func (f *fakeStore) Apply(_ context.Context, _, _ string, entries []Entry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applied = append(f.applied, entries...)
	return nil
}

func (f *fakeStore) Drop(_ context.Context, _ string, paths []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dropped = append(f.dropped, paths...)
	return nil
}

func (f *fakeStore) Changes(_ context.Context, _ string) ([]cache.Change, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.changes, f.changesErr
}

func (f *fakeStore) Pull(_ context.Context, _ string, paths []string, into func(File) error) error {
	f.mu.Lock()
	f.pulled = append(f.pulled, paths...)
	f.mu.Unlock()

	for _, p := range paths {
		file, ok := f.files[p]
		if !ok {
			continue
		}
		if err := into(file); err != nil {
			return err
		}
	}
	return nil
}

// cacheWith returns a Cache wired to the store, with no logging.
func cacheWith(t *testing.T, store *fakeStore) *Cache {
	t.Helper()
	c := &Cache{
		Store: func() (Store, bool) { return store, true },
		Ctx:   t.Context(),
	}
	t.Cleanup(c.Stop)
	return c
}

// The store names the paths that come back, and a store is not this machine's
// to trust with one: a tar entry called ../../.ssh/authorized_keys would
// otherwise be written wherever it pointed. Nothing tested this at all before
// the policy could be driven without a session.
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
		changes: []cache.Change{
			{Path: "/main.go", Size: 6, ModTime: changedAt.UnixNano()},
		},
		files: map[string]File{
			"/main.go": {Path: "/main.go", ModTime: changedAt, Mode: 0o644, Body: strings.NewReader("after\n")},
		},
	}

	c := cacheWith(t, store)
	c.shares.set("/cwd", root, &fillState{Done: true, Cached: true})
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
	c.shares.set("/cwd", t.TempDir(), &fillState{Done: true, Cached: true})

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
		changes: []cache.Change{{Path: "/main.go", Size: 1, ModTime: 1}},
	}
	c := cacheWith(t, store)
	c.shares.set("/cwd", t.TempDir(), &fillState{Done: false})

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

// A fill runs once per share. A second consumer of the same directory finding
// it already running is the ordinary case, and re-sending the tree would cost
// the link twice for nothing.
func TestFillRunsOncePerShare(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.go", "b.go"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("package x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	store := &fakeStore{}
	c := cacheWith(t, store)

	c.Fill("/cwd", root)
	waitFor(t, func() bool { r := c.Reports(); return len(r) == 1 && r[0].Done })

	c.Fill("/cwd", root)
	waitFor(t, func() bool { return len(c.Reports()) == 1 })

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.applied) != 2 {
		t.Errorf("applied %d entries over two fills, want the tree once", len(store.applied))
	}
}

// A fill with nowhere to send its batches is not an error: the share is served
// from the authoritative tree meanwhile, and the next fill starts from scratch.
func TestFillWithNoStore(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := &Cache{Store: func() (Store, bool) { return nil, false }, Ctx: t.Context()}
	t.Cleanup(c.Stop)

	c.Fill("/cwd", root)
	waitFor(t, func() bool { r := c.Reports(); return len(r) == 1 && r[0].Done })

	if err := c.Reports()[0].Err; err != nil {
		t.Errorf("a fill with no store reported %v, want it to pass quietly", err)
	}
}

func waitFor(t *testing.T, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the fill")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
