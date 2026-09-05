package nfsserve

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// The link count on the wire is the real one. It used to be 1 for everything,
// so a directory showed fewer links than it had subdirectories and a
// hard-linked file showed one name where it had two, both of which a tool
// walking the tree (find -noleaf logic, rsync -H) reads as meaning something.
func TestLinkCountIsReported(t *testing.T) {
	dir := t.TempDir()
	for _, sub := range []string{"a", "b"} {
		if err := os.MkdirAll(filepath.Join(dir, "parent", sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "one"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(dir, "one"), filepath.Join(dir, "two")); err != nil {
		t.Skipf("no hard links here: %v", err)
	}

	r := NewRegistry(DefaultAttrs)
	if _, err := r.RegisterCWD(dir); err != nil {
		t.Fatal(err)
	}
	target := mustMount(t, serve(t, r), "/cwd")

	parent, err := target.Getattr("parent")
	if err != nil {
		t.Fatal(err)
	}
	// Unix counts `.`, `..` and every subdirectory's `..`; NTFS counts a
	// directory as one name, and reporting that is still the truth.
	want := uint32(3)
	if runtime.GOOS == "windows" {
		want = 1
	}
	if parent.Nlink < want {
		t.Errorf("a directory with two subdirectories reports nlink=%d on %s, want >= %d",
			parent.Nlink, runtime.GOOS, want)
	}

	for _, name := range []string{"one", "two"} {
		fi, err := target.Getattr(name)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Nlink != 2 {
			t.Errorf("%s, one of two names for a file, reports nlink=%d, want 2", name, fi.Nlink)
		}
	}
}
