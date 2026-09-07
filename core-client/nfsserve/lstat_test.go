package nfsserve

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/go-git/go-billy/v5/osfs"
)

// Lstat reports the link, not what it points at. That is the whole reason
// noFollowFS exists, and it now resolves the path itself rather than through
// the bound osfs, so it is worth saying out loud that it still holds.
func TestLstatReportsTheLinkItself(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a symlink needs a privilege an ordinary account does not hold")
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "target"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	fs := shareFSOver(dir, "")

	fi, err := fs.Lstat("link")
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("Lstat(link) reports %v; it followed the link", fi.Mode())
	}

	// Stat, which does follow, must still see the file at the end of it.
	if fi, err = fs.Stat("link"); err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Error("Stat(link) reports a symlink; it should follow to the file")
	}
}

// A directory is named by every spelling that cleans to it, the share root
// above all: it has no last element, which is the one case secureLeaf refuses
// on its own, and a share whose root cannot be stat'ed cannot be mounted.
func TestLstatOfADirectoryByEverySpelling(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	fs := shareFSOver(dir, "")

	root, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", ".", "./", string(filepath.Separator), "sub/.."} {
		fi, err := fs.Lstat(filepath.FromSlash(name))
		if err != nil {
			t.Errorf("Lstat(%q): %v", name, err)
			continue
		}
		if !os.SameFile(fi, root) {
			t.Errorf("Lstat(%q) reports %q (%v), want the share directory", name, fi.Name(), fi.Mode())
		}
	}

	// `sub/.` has a base of `.`, so the name reaches secureLeaf cleaned or is
	// refused by it.
	sub, err := os.Lstat(filepath.Join(dir, "sub"))
	if err != nil {
		t.Fatal(err)
	}
	fi, err := fs.Lstat(filepath.FromSlash("sub/."))
	if err != nil {
		t.Fatalf("Lstat(sub/.): %v", err)
	}
	if !os.SameFile(fi, sub) {
		t.Errorf("Lstat(sub/.) reports %q (%v), want the subdirectory", fi.Name(), fi.Mode())
	}
}

// A name that climbs out of the share reports the share's own file or nothing,
// never the one above it. SecureJoin CLAMPS such a name to the share root
// rather than refusing it, so the file INSIDE the share is what makes this
// assert anything: without it every spelling misses, and a miss is what a
// broken resolution returns too.
func TestLstatCannotReachOutsideTheShare(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(filepath.Dir(dir), "outside.txt")
	if err := os.WriteFile(outside, []byte("above the share"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(outside) })
	if err := os.WriteFile(filepath.Join(dir, "outside.txt"), []byte("inside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	inside, err := os.Lstat(filepath.Join(dir, "outside.txt"))
	if err != nil {
		t.Fatal(err)
	}
	fs := shareFSOver(dir, "")

	// An absolute path above the share is refused outright: nothing makes it
	// relative to the root, so there is nothing to clamp.
	if fi, err := fs.Lstat(outside); err == nil {
		t.Errorf("Lstat(%q) returned %d bytes; an absolute path above the share must be refused", outside, fi.Size())
	}

	for _, name := range []string{"../outside.txt", "../../outside.txt", "sub/../../outside.txt"} {
		fi, err := fs.Lstat(filepath.FromSlash(name))
		if err != nil {
			t.Errorf("Lstat(%q) = %v, want it clamped to the share's own outside.txt", name, err)
			continue
		}
		if !os.SameFile(fi, inside) {
			t.Errorf("Lstat(%q) reported a file of %d bytes, want the share's own outside.txt (%d bytes) and never the one above", name, fi.Size(), inside.Size())
		}
	}
}

// What the change is for. Not a gate: a timing assertion on a loaded runner is
// a flake. It reports, so the number in Lstat's comment can be re-measured.
func BenchmarkLstat(b *testing.B) {
	dir := b.TempDir()
	deep := filepath.Join(dir, "a", "b", "c", "d")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deep, "f"), []byte("hi"), 0o644); err != nil {
		b.Fatal(err)
	}
	name := filepath.Join("a", "b", "c", "d", "f")

	b.Run("share", func(b *testing.B) {
		fs := shareFSOver(dir, "")
		for b.Loop() {
			if _, err := fs.Lstat(name); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("bound-osfs", func(b *testing.B) {
		fs := osfs.New(dir, osfs.WithBoundOS())
		for b.Loop() {
			if _, err := fs.Lstat(name); err != nil {
				b.Fatal(err)
			}
		}
	})
}
