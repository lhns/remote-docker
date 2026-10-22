package dircache

// How write-back looks for a name that differs only in case from one here
// (ADR 0044): what it refuses, and what that costs.

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// caseInsensitive reports whether root's filesystem treats two spellings as one.
func caseInsensitive(t *testing.T, root string) bool {
	t.Helper()
	probe := filepath.Join(root, "Probe-Case")
	if err := os.WriteFile(probe, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(probe) }()
	_, err := os.Stat(filepath.Join(root, "probe-case"))
	return err == nil
}

// sentHere writes a file here and returns the baseline a fill would have
// recorded for it.
func sentHere(t *testing.T, root, rel, body string) Baseline {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	b, ok := baselineOf(root, rel)
	if !ok {
		t.Fatalf("no baseline for %s", rel)
	}
	return b
}

// A file this machine has, renamed here by case only, is not written back
// under its old spelling: on a filesystem that treats the two as one that
// would put the container's contents under the name the user chose.
func TestAFileRenamedHereByCaseOnlyIsNotWrittenOver(t *testing.T) {
	root := t.TempDir()
	insensitive := caseInsensitive(t, root)
	sent := sentHere(t, root, "license", "mine")
	if err := os.Rename(filepath.Join(root, "license"), filepath.Join(root, "LICENSE")); err != nil {
		t.Fatal(err)
	}

	store := &faultStore{}
	store.add("/license", "the container's")
	c, _, clk := wbCache(t, store, root)
	c.shares.set(wbShare, root, &shareState{Report: Report{Done: true}, Cached: true}, map[string]Baseline{"/license": sent})
	rounds(t, c, clk, 2)

	if got, _ := os.ReadFile(filepath.Join(root, "LICENSE")); string(got) != "mine" {
		t.Errorf("LICENSE holds %q, want it untouched", got)
	}
	if !insensitive {
		return
	}
	sk := c.Reports()[0].WriteBack.Skipped
	if len(sk) != 1 || sk[0].Path != "/license" || sk[0].Kind != SkipClash {
		t.Errorf("status names %+v, want /license as a collision", sk)
	}
}

// Two new files that differ only in case, in one pass: on a filesystem that
// treats them as one, the second is a collision with the first, even though
// the directory was looked at before the first landed. Where the filesystem
// tells them apart, both land.
func TestTwoNamesDifferingInCaseInOnePassAreACollision(t *testing.T) {
	root := t.TempDir()
	insensitive := caseInsensitive(t, root)
	// Landing notes looks at the directory before either Makefile exists.
	sent := sentHere(t, root, "notes", "mine")

	store := &faultStore{}
	store.add("/notes", "the container's notes")
	store.add("/Makefile", "upper")
	store.add("/makefile", "lower")
	c, _, clk := wbCache(t, store, root)
	c.shares.set(wbShare, root, &shareState{Report: Report{Done: true}, Cached: true}, map[string]Baseline{"/notes": sent})
	rounds(t, c, clk, 1)

	if got, _ := os.ReadFile(filepath.Join(root, "notes")); string(got) != "the container's notes" {
		t.Errorf("notes holds %q, want the container's", got)
	}
	if !insensitive {
		for name, want := range map[string]string{"Makefile": "upper", "makefile": "lower"} {
			if got, _ := os.ReadFile(filepath.Join(root, name)); string(got) != want {
				t.Errorf("%s holds %q, want %q", name, got, want)
			}
		}
		return
	}
	if got, _ := os.ReadFile(filepath.Join(root, "Makefile")); string(got) != "upper" {
		t.Errorf("Makefile holds %q, want the first of the two", got)
	}
	sk := c.Reports()[0].WriteBack.Skipped
	if len(sk) != 1 || sk[0].Path != "/makefile" || sk[0].Kind != SkipClash {
		t.Errorf("status names %+v, want /makefile as a collision", sk)
	}
}

// A pass lists a directory once for its case checks, however many files it
// lands there: a listing per file is quadratic in the size of the directory.
func TestAPassListsADirectoryOnceForCaseClashes(t *testing.T) {
	const n = 20
	root := t.TempDir()
	store := &faultStore{}
	filled := map[string]Baseline{}
	for i := range n {
		rel := fmt.Sprintf("src/f%02d.go", i)
		filled["/"+rel] = sentHere(t, root, rel, "mine")
		store.add("/"+rel, "the container's")
	}
	c, _, clk := wbCache(t, store, root)
	c.shares.set(wbShare, root, &shareState{Report: Report{Done: true}, Cached: true}, filled)

	var mu sync.Mutex
	listed := map[string]int{}
	c.listDir = func(r *os.Root, dir string) ([]string, error) {
		mu.Lock()
		listed[filepath.ToSlash(filepath.Clean(dir))]++
		mu.Unlock()
		return readDirNames(r, dir)
	}
	rounds(t, c, clk, 1)

	for rel := range filled {
		if got, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel[1:]))); string(got) != "the container's" {
			t.Errorf("%s holds %q, want the container's", rel, got)
		}
	}
	if listed["src"] != 1 {
		t.Errorf("src was listed %d times in one pass, want once (all listings: %v)", listed["src"], listed)
	}
}
