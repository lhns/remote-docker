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
	fs := shareFS(dir, "")

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

// The share root has no last element to leave alone, which is the one case
// secureLeaf refuses on its own: a share whose root cannot be stat'ed cannot
// be mounted at all.
func TestLstatOfTheShareRoot(t *testing.T) {
	dir := t.TempDir()
	fs := shareFS(dir, "")

	for _, name := range []string{"", ".", string(filepath.Separator)} {
		fi, err := fs.Lstat(name)
		if err != nil {
			t.Errorf("Lstat(%q): %v", name, err)
			continue
		}
		if !fi.IsDir() {
			t.Errorf("Lstat(%q) reports %v, want the share directory", name, fi.Mode())
		}
	}
}

// A name that climbs out of the share must not report a file outside it,
// whatever spelling it arrives in.
func TestLstatCannotReachOutsideTheShare(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(filepath.Dir(dir), "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(outside) })

	fs := shareFS(dir, "")
	for _, name := range []string{
		filepath.Join("..", "outside.txt"),
		filepath.Join("sub", "..", "..", "outside.txt"),
		outside,
	} {
		if fi, err := fs.Lstat(name); err == nil {
			t.Errorf("Lstat(%q) returned %v, want a refusal or a miss", name, fi.Name())
		}
	}
}

// What the change is for. Not a gate: a timing assertion on a loaded runner is
// a flake. It reports, so the number can be checked when it matters.
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
		fs := shareFS(dir, "")
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
