package nfsserve

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	nfsclient "github.com/willscott/go-nfs-client/nfs"
)

// linkedShare is a mounted share holding a file `b` and a symlink `a -> b`.
func linkedShare(t *testing.T) (string, *nfsclient.Target) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is privileged on this platform")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "b"), []byte("the target"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("b", filepath.Join(dir, "a")); err != nil {
		t.Fatal(err)
	}
	r := NewRegistry(DefaultAttrs)
	if _, err := r.RegisterCWD(dir); err != nil {
		t.Fatal(err)
	}
	return dir, mustMount(t, serve(t, r), "/cwd")
}

// Removing a symlink through the share removes the LINK. go-billy's BoundOS
// resolved the last component too, so `rm a` deleted b and left a dangling:
// somebody's file gone for removing a name that pointed at it.
func TestRemovingASymlinkThroughTheShareKeepsItsTarget(t *testing.T) {
	dir, target := linkedShare(t)

	if err := target.Remove("a"); err != nil {
		t.Fatalf("removing the link: %v", err)
	}

	if got, err := os.ReadFile(filepath.Join(dir, "b")); err != nil || string(got) != "the target" {
		t.Errorf("the link's target was touched: content %q, err %v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "a")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the link is still there: Lstat err = %v", err)
	}
}

// Renaming a symlink moves the link, and what it points at stays put.
func TestRenamingASymlinkThroughTheShareMovesTheLink(t *testing.T) {
	dir, target := linkedShare(t)

	if err := target.Rename("a", "c"); err != nil {
		t.Fatalf("renaming the link: %v", err)
	}

	if got, err := os.ReadFile(filepath.Join(dir, "b")); err != nil || string(got) != "the target" {
		t.Errorf("the link's target was moved or changed: content %q, err %v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "a")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the old name is still there: Lstat err = %v", err)
	}
	fi, err := os.Lstat(filepath.Join(dir, "c"))
	if err != nil {
		t.Fatalf("the new name is missing: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the new name is a %v, not a symlink", fi.Mode())
	}
	if dest, err := os.Readlink(filepath.Join(dir, "c")); err != nil || dest != "b" {
		t.Errorf("the moved link points at %q (err %v), want b", dest, err)
	}
}

// A plain file is still removed and renamed through the share, and a
// subdirectory mount, which is a Chroot, keeps the same behaviour.
func TestRemoveAndRenameOfAPlainFileThroughTheShare(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"one", "sub/two"} {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(name)), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r := NewRegistry(DefaultAttrs)
	if _, err := r.RegisterCWD(dir); err != nil {
		t.Fatal(err)
	}
	addr := serve(t, r)
	root := mustMount(t, addr, "/cwd")
	sub := mustMount(t, addr, "/cwd/sub")

	if err := root.Rename("one", "sub/moved"); err != nil {
		t.Fatalf("rename across directories: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "sub", "moved")); err != nil || string(got) != "one" {
		t.Errorf("after the rename sub/moved holds %q, err %v", got, err)
	}
	if err := root.Remove("sub/moved"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "sub", "moved")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("sub/moved is still there: %v", err)
	}

	if err := sub.Rename("two", "three"); err != nil {
		t.Fatalf("rename inside a subdirectory mount: %v", err)
	}
	if err := sub.Remove("three"); err != nil {
		t.Fatalf("remove inside a subdirectory mount: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "sub", "three")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("sub/three is still there: %v", err)
	}
}

// The last element must be one plain name, and the directory part cannot leave
// the share: a `..` there resolves to the share root, never above it.
func TestNoFollowStaysInsideTheShare(t *testing.T) {
	base := t.TempDir()
	share := filepath.Join(base, "share")
	if err := os.MkdirAll(filepath.Join(share, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside")
	if err := os.WriteFile(outside, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	fs := shareFS(share, "")

	for _, name := range []string{".", "..", "sub/..", "sub/.", "/", "", "sub/"} {
		if err := fs.Remove(name); err == nil {
			t.Errorf("Remove(%q) succeeded; it does not name an entry", name)
		}
		if err := fs.Rename("sub", name); err == nil {
			t.Errorf("Rename(sub, %q) succeeded; it does not name an entry", name)
		}
	}

	// The directory part is contained by SecureJoin, which resolves `..` at
	// the share root to the root itself: both of these name <share>/outside
	// and never <base>/outside, so the remove finds nothing and the rename
	// lands INSIDE the share.
	if err := fs.Remove("../outside"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Remove(../outside) = %v, want not-exist inside the share", err)
	}
	if err := fs.Rename("sub", "../../outside"); err != nil {
		t.Errorf("Rename(sub, ../../outside) = %v, want it contained to the share root", err)
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "keep" {
		t.Errorf("a file outside the share was touched: %q, %v", got, err)
	}
	if fi, err := os.Stat(filepath.Join(share, "outside")); err != nil || !fi.IsDir() {
		t.Errorf("the contained rename did not land at <share>/outside: %v", err)
	}
	if _, err := os.Stat(share); err != nil {
		t.Errorf("the share root itself was touched: %v", err)
	}
}

// A single-file share removes and renames its one file as before, and still
// refuses to touch a sibling.
func TestSingleFileShareRemovesAndRenamesItsFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "only.conf")
	for _, name := range []string{"only.conf", "sibling"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fs := shareFS(dir, "only.conf")

	if err := fs.Rename("only.conf", "sibling"); !errors.Is(err, os.ErrPermission) {
		t.Errorf("Rename onto a sibling = %v, want permission denied", err)
	}
	if err := fs.Remove("sibling"); !errors.Is(err, os.ErrPermission) {
		t.Errorf("Remove of a sibling = %v, want permission denied", err)
	}
	if err := fs.Remove("only.conf"); err != nil {
		t.Fatalf("Remove of the file: %v", err)
	}
	if _, err := os.Lstat(file); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the file is still there: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "sibling")); err != nil || string(got) != "sibling" {
		t.Errorf("the sibling was touched: %q, %v", got, err)
	}
}
