package fswatch

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/fsnotify/fsnotify"
)

// fakeBackend records what the tree asked for and lets a test inject events,
// so the whole of the bookkeeping is exercised with no kernel watches at all.
type fakeBackend struct {
	mu      sync.Mutex
	added   []string
	removed []string
	failAdd map[string]error

	events chan fsnotify.Event
	errors chan error
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		failAdd: map[string]error{},
		events:  make(chan fsnotify.Event, 256),
		errors:  make(chan error, 8),
	}
}

func (f *fakeBackend) Add(p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failAdd[p]; err != nil {
		return err
	}
	// fsnotify refuses a path that is not there, and so must this: a fake
	// more permissive than the real thing turns "the code handles a vanished
	// directory" into a claim no test has actually checked.
	if _, err := os.Lstat(p); err != nil {
		return err
	}
	f.added = append(f.added, p)
	return nil
}

func (f *fakeBackend) Remove(p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, p)
	return nil
}

func (f *fakeBackend) Events() <-chan fsnotify.Event { return f.events }
func (f *fakeBackend) Errors() <-chan error          { return f.errors }
func (f *fakeBackend) Close() error                  { close(f.events); return nil }

func (f *fakeBackend) addedSet() map[string]bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]bool{}
	for _, p := range f.added {
		out[p] = true
	}
	return out
}

func (f *fakeBackend) removedList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.removed)
}

// mkdirs builds a directory tree under root and returns root.
func mkdirs(t *testing.T, root string, dirs ...string) string {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func newTestTree(t *testing.T, be backend, budget int, exclude []string) *tree {
	t.Helper()
	return newTree(runtime.GOOS, be, budget, exclude, nil)
}

func TestSyncWalksTheWholeShare(t *testing.T) {
	root := mkdirs(t, t.TempDir(), "src", "src/app", "docs")
	be := newFakeBackend()
	tr := newTestTree(t, be, 100, nil)

	tr.sync([]Share{{ExportPath: "/cwd", LocalPath: root}})

	added := be.addedSet()
	for _, want := range []string{root,
		filepath.Join(root, "src"),
		filepath.Join(root, "src", "app"),
		filepath.Join(root, "docs"),
	} {
		if !added[want] {
			t.Errorf("did not watch %s", want)
		}
	}
	if len(added) != 4 {
		t.Errorf("watched %d directories, want 4: %v", len(added), added)
	}
}

func TestExcludedDirectoriesAreNotWatched(t *testing.T) {
	root := mkdirs(t, t.TempDir(), "src", "node_modules", "node_modules/pkg", ".git", ".git/objects")
	be := newFakeBackend()
	tr := newTestTree(t, be, 100, DefaultExcludes)

	tr.sync([]Share{{ExportPath: "/cwd", LocalPath: root}})

	added := be.addedSet()
	if added[filepath.Join(root, "node_modules")] || added[filepath.Join(root, ".git")] {
		t.Errorf("watched an excluded directory: %v", added)
	}
	if !added[filepath.Join(root, "src")] {
		t.Error("did not watch src")
	}
	if tr.excluded != 2 {
		t.Errorf("counted %d exclusions, want 2", tr.excluded)
	}
}

// A build output directory must NOT be excluded by default: a container
// serving dist/ reloading when dist/ changes is exactly the workflow this
// exists for, and excluding it would reintroduce ADR 0014's silent
// nothing-happens somewhere harder to find.
func TestBuildOutputsAreNotExcludedByDefault(t *testing.T) {
	for _, name := range []string{"dist", "build", "target", "out", "vendor"} {
		if slices.Contains(DefaultExcludes, name) {
			t.Errorf("%q is excluded by default; a watcher on it would silently never fire", name)
		}
	}
}

// The walk is breadth-first so that running out of budget means the DEEP part
// of the tree is uncovered, never a random half. Depth-first would spend the
// whole budget inside the first subtree it happened to sort into.
func TestBudgetKeepsShallowDirectoriesFirst(t *testing.T) {
	root := mkdirs(t, t.TempDir(),
		"a", "a/deep", "a/deep/deeper",
		"b", "b/deep",
		"c",
	)
	be := newFakeBackend()
	tr := newTestTree(t, be, 4, nil) // root + three children

	tr.sync([]Share{{ExportPath: "/cwd", LocalPath: root}})

	added := be.addedSet()
	if len(added) != 4 {
		t.Fatalf("watched %d directories, want exactly the budget of 4: %v", len(added), added)
	}
	for _, want := range []string{root, filepath.Join(root, "a"), filepath.Join(root, "b"), filepath.Join(root, "c")} {
		if !added[want] {
			t.Errorf("budget spent somewhere deeper than %s: %v", want, added)
		}
	}
	if tr.deniedN == 0 {
		t.Error("the budget was exhausted but nothing was recorded as denied")
	}
	if len(tr.denied) == 0 {
		t.Error("no denied subtree was named; a cap must never be silent")
	}
}

// Refusals below an already-reported directory fold into it, so the report
// stays a report rather than a list of every directory in the tree.
func TestDenialsFoldIntoTheShallowestSubtree(t *testing.T) {
	be := newFakeBackend()
	tr := newTestTree(t, be, 0, nil)

	tr.deny(filepath.Join("/x", "a"))
	tr.deny(filepath.Join("/x", "a", "b"))
	tr.deny(filepath.Join("/x", "a", "b", "c"))

	if len(tr.denied) != 1 {
		t.Errorf("recorded %d subtrees, want 1: %v", len(tr.denied), tr.denied)
	}
	if tr.deniedN != 3 {
		t.Errorf("counted %d denials, want 3", tr.deniedN)
	}
	for _, n := range tr.denied {
		if n != 3 {
			t.Errorf("subtree count = %d, want 3", n)
		}
	}
}

// A sibling is not a descendant. "/x/ab" must not fold into "/x/a".
func TestDenialsDoNotFoldIntoASiblingPrefix(t *testing.T) {
	be := newFakeBackend()
	tr := newTestTree(t, be, 0, nil)

	tr.deny("/x/a")
	tr.deny("/x/ab")

	if len(tr.denied) != 2 {
		t.Errorf("recorded %d subtrees, want 2: %v", len(tr.denied), tr.denied)
	}
}

// The asymmetry that is easy to get wrong. A rename must prune the subtree,
// because watches follow the inode and every later event would be spelled with
// the old path. A delete must not, because the kernel sends one event per
// watched descendant and a prefix scan would make `rm -rf` quadratic.
func TestRenamePrunesTheSubtreeAndRemoveDoesNot(t *testing.T) {
	root := mkdirs(t, t.TempDir(), "keep", "gone", "gone/inner", "gone/inner/deepest")
	be := newFakeBackend()
	tr := newTestTree(t, be, 100, nil)
	tr.sync([]Share{{ExportPath: "/cwd", LocalPath: root}})

	gone := filepath.Join(root, "gone")
	inner := filepath.Join(gone, "inner")

	if !tr.watching(inner) {
		t.Fatal("setup: inner was not watched")
	}

	// Remove drops only the named directory.
	tr.removeOne(gone)
	if tr.watching(gone) {
		t.Error("removeOne left the directory watched")
	}
	if !tr.watching(inner) {
		t.Error("removeOne pruned a descendant; the kernel sends its own event for that")
	}

	// Rename drops the whole subtree.
	tr.removeTree(gone)
	for _, p := range []string{inner, filepath.Join(inner, "deepest")} {
		if tr.watching(p) {
			t.Errorf("removeTree left %s watched; later events would carry a stale path", p)
		}
	}
	if !tr.watching(filepath.Join(root, "keep")) {
		t.Error("removeTree pruned an unrelated sibling")
	}
}

// The race that cannot be closed and must therefore be recovered from:
// between a directory being created and our watch landing, children can appear
// whose events are never delivered, because no watch existed.
func TestAddTreeEmitsWhatItFindsForALateWatch(t *testing.T) {
	root := t.TempDir()
	be := newFakeBackend()
	tr := newTestTree(t, be, 100, nil)
	tr.sync([]Share{{ExportPath: "/cwd", LocalPath: root}})

	// The whole subtree exists before we are told about the top of it.
	created := mkdirs(t, root, "late", "late/inner")
	for _, f := range []string{"late/a.go", "late/inner/b.go"} {
		if err := os.WriteFile(filepath.Join(created, filepath.FromSlash(f)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var found []string
	tr.addTree(tr.roots[0], filepath.Join(root, "late"), func(p string, isDir bool) {
		rel, _ := relativeTo(runtime.GOOS, tr.roots[0].parts, p)
		found = append(found, rel)
	})
	sort.Strings(found)

	want := []string{"/late/a.go", "/late/inner", "/late/inner/b.go"}
	if !slices.Equal(found, want) {
		t.Errorf("synthesised %v, want %v -- anything missing here is lost forever", found, want)
	}
}

// A directory that vanishes mid-walk is ordinary, not an error: its own
// removal event is already on the way.
func TestAddTreeToleratesAVanishedDirectory(t *testing.T) {
	root := t.TempDir()
	be := newFakeBackend()
	tr := newTestTree(t, be, 100, nil)
	tr.sync([]Share{{ExportPath: "/cwd", LocalPath: root}})

	tr.addTree(tr.roots[0], filepath.Join(root, "never-existed"), nil)
	// No panic, and nothing bogus recorded as watched.
	if tr.watching(filepath.Join(root, "never-existed")) {
		t.Error("a non-existent directory was recorded as watched")
	}
}

func TestSyncRemovesAShareThatWentAway(t *testing.T) {
	a := mkdirs(t, t.TempDir(), "sub")
	b := t.TempDir()
	be := newFakeBackend()
	tr := newTestTree(t, be, 100, nil)

	tr.sync([]Share{{ExportPath: "/cwd", LocalPath: a}, {ExportPath: "/m/0123456789abcdef", LocalPath: b}})
	if !tr.watching(b) {
		t.Fatal("setup: second share not watched")
	}

	tr.sync([]Share{{ExportPath: "/cwd", LocalPath: a}})
	if tr.watching(b) {
		t.Error("a removed share is still watched")
	}
	if !tr.watching(filepath.Join(a, "sub")) {
		t.Error("removing one share disturbed another")
	}
	if len(tr.roots) != 1 {
		t.Errorf("kept %d roots, want 1", len(tr.roots))
	}
}

// Sync is called from both the registration path and a periodic reconcile, so
// repeating it must not re-add watches or grow anything.
func TestSyncIsIdempotent(t *testing.T) {
	root := mkdirs(t, t.TempDir(), "src")
	be := newFakeBackend()
	tr := newTestTree(t, be, 100, nil)
	shares := []Share{{ExportPath: "/cwd", LocalPath: root}}

	tr.sync(shares)
	first := len(be.addedSet())
	tr.sync(shares)
	tr.sync(shares)

	if got := len(be.addedSet()); got != first {
		t.Errorf("watched %d directories after three syncs, want %d", got, first)
	}
	if len(tr.roots) != 1 {
		t.Errorf("kept %d roots after three syncs, want 1", len(tr.roots))
	}
	if len(be.removedList()) != 0 {
		t.Errorf("a repeated sync removed watches: %v", be.removedList())
	}
}

func TestRootForFindsTheOwningShare(t *testing.T) {
	a := t.TempDir()
	b := t.TempDir()
	be := newFakeBackend()
	tr := newTestTree(t, be, 100, nil)
	tr.sync([]Share{{ExportPath: "/cwd", LocalPath: a}, {ExportPath: "/m/0123456789abcdef", LocalPath: b}})

	root, rel, ok := tr.rootFor(filepath.Join(b, "x", "y.go"))
	if !ok || root.export != "/m/0123456789abcdef" || rel != "/x/y.go" {
		t.Errorf("rootFor = %v, %q, %v; want the second share and /x/y.go", root, rel, ok)
	}

	if _, _, ok := tr.rootFor(filepath.Join(t.TempDir(), "elsewhere")); ok {
		t.Error("rootFor claimed a path outside every share")
	}
}

func TestDirKeyFoldsCaseOnlyWhereTheFilesystemDoes(t *testing.T) {
	if dirKey("windows", `C:\Projects\Foo`) != dirKey("windows", `c:\projects\foo`) {
		t.Error("windows keys did not fold case")
	}
	if dirKey("linux", "/a/B") == dirKey("linux", "/a/b") {
		t.Error("linux keys folded case, which would merge two different directories")
	}
	// Whatever it does, it must not be used for a wire path.
	if !strings.HasPrefix(dirKey("linux", "/a/b"), "/") {
		t.Error("dirKey lost the leading separator")
	}
}

// writeFile creates a file with a byte of content, for tests that need
// something for the walk to find.
func writeFile(p string) error { return os.WriteFile(p, []byte("x"), 0o644) }

// A single-file share watches the containing directory and nothing below it:
// the siblings are not exported, and their subtrees are not ours to spend the
// budget on (ADR 0039).
func TestASingleFileShareWatchesOnlyItsDirectory(t *testing.T) {
	root := mkdirs(t, t.TempDir(), "conf", "conf/sites", "conf/sites/deep")
	conf := filepath.Join(root, "conf", "nginx.conf")
	if err := os.WriteFile(conf, []byte("server {}"), 0o644); err != nil {
		t.Fatal(err)
	}
	be := newFakeBackend()
	tr := newTestTree(t, be, 100, nil)

	tr.sync([]Share{{ExportPath: "/m/abc", LocalPath: conf, File: "nginx.conf"}})

	added := be.addedSet()
	dir := filepath.Join(root, "conf")
	if !added[dir] {
		t.Errorf("did not watch %s, where the file's events arrive", dir)
	}
	if len(added) != 1 {
		t.Errorf("watched %v, want only the containing directory", added)
	}
}

// Everything but the exported name is dropped, so a sibling being edited does
// not reach the workspace as a change to a share it is not in.
func TestASingleFileShareReportsOnlyItsOwnFile(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "nginx.conf")
	if err := os.WriteFile(conf, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	tr := newTestTree(t, newFakeBackend(), 100, nil)
	tr.sync([]Share{{ExportPath: "/m/abc", LocalPath: conf, File: "nginx.conf"}})

	if _, rel, ok := tr.rootFor(conf); !ok || rel != "/nginx.conf" {
		t.Errorf("rootFor(the file) = %q, %v; want /nginx.conf, true", rel, ok)
	}
	if _, _, ok := tr.rootFor(filepath.Join(dir, "secret.env")); ok {
		t.Error("a sibling of the exported file was reported as part of the share")
	}
}
