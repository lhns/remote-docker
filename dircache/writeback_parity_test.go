package dircache

// A write=back share behaves like a bind mount: what the container changes
// comes back, last writer wins, and the side that lost is said (ADR 0044).
// These cases are files the cache never held, which is every existing file
// while prefetch is off.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// An existing file the container rewrote, and nobody touched here since,
// arrives. The cache never held it: with prefetch off that is every file.
func TestAnExistingFileTheContainerRewroteIsCarriedBack(t *testing.T) {
	root := t.TempDir()
	writeLocal(t, root, "index.json", "{}\n")

	store := &faultStore{}
	store.add("/index.json", "{\"from\":\"the container\"}\n")
	c, _, clk := wbCache(t, store, root)
	rounds(t, c, clk, 1)

	if got, _ := os.ReadFile(filepath.Join(root, "index.json")); string(got) != "{\"from\":\"the container\"}\n" {
		t.Errorf("index.json holds %q, want the container's version", got)
	}
}

// A file changed here after the container's change is this machine's: it is
// kept, and the container's version is named in status rather than dropped
// without a word.
func TestAnExistingFileChangedHereLaterIsKeptAndReported(t *testing.T) {
	root := t.TempDir()
	writeLocal(t, root, "out.txt", "written here last\n")

	store := &faultStore{mtime: time.Now().Add(-time.Hour).Truncate(time.Second)}
	store.add("/out.txt", "written in the container first\n")
	c, _, clk := wbCache(t, store, root)
	rounds(t, c, clk, 2)

	if got, _ := os.ReadFile(filepath.Join(root, "out.txt")); string(got) != "written here last\n" {
		t.Errorf("out.txt holds %q, want this machine's later version kept", got)
	}
	reports := c.Reports()
	if len(reports) != 1 {
		t.Fatalf("reports = %+v, want one share", reports)
	}
	var named bool
	for _, sk := range reports[0].WriteBack.Skipped {
		if sk.Path == "/out.txt" {
			named = true
		}
	}
	if !named {
		t.Errorf("status does not name the container's change that was not carried back: %+v", reports[0].WriteBack)
	}
}

// A copy is not a change. An entrypoint's chown -R copies every file into the
// cache layer with its size and time unchanged; reading that as the container
// writing would rewrite the whole tree.
func TestACopyOfAnExistingFileIsNotAChange(t *testing.T) {
	root := t.TempDir()
	writeLocal(t, root, "lib/util.go", "package lib\n")
	info, err := os.Stat(filepath.Join(root, "lib", "util.go"))
	if err != nil {
		t.Fatal(err)
	}

	store := &faultStore{content: map[string]string{"/lib/util.go": "package lib\n"}, mtime: info.ModTime()}
	store.changes = []Change{{Path: "/lib/util.go", Size: info.Size(), ModTime: info.ModTime().UnixNano()}}
	c, _, clk := wbCache(t, store, root)
	rounds(t, c, clk, 2)

	if len(store.pulled) != 0 {
		t.Errorf("a copy of an unchanged file was fetched: %v", store.pulled)
	}
	after, err := os.Stat(filepath.Join(root, "lib", "util.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(info.ModTime()) {
		t.Errorf("the file was rewritten: mtime %v, was %v", after.ModTime(), info.ModTime())
	}
}

// A cache the fill could not finish (prefetch ran out of budget) still carries
// back. What the fill never sent is decided like any other file the cache never
// held.
func TestAnIncompleteCacheStillCarriesBack(t *testing.T) {
	root := t.TempDir()
	writeLocal(t, root, "index.json", "{}\n")

	store := &faultStore{}
	store.add("/index.json", "rewritten in the container\n")
	store.add("/made-there.txt", "new in the container\n")
	c, _, clk := wbCache(t, store, root)
	c.shares.set(wbShare, root, &shareState{Report: Report{Done: true}, Cached: false}, nil)
	rounds(t, c, clk, 1)

	for rel, want := range map[string]string{
		"index.json":     "rewritten in the container\n",
		"made-there.txt": "new in the container\n",
	} {
		got, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil || string(got) != want {
			t.Errorf("%s holds %q (%v), want %q", rel, got, err, strings.TrimSpace(want))
		}
	}
}
