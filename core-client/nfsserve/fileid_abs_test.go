package nfsserve

import (
	"os"
	"path/filepath"
	"testing"

	nfsfile "github.com/willscott/go-nfs/file"

	"github.com/lhns/remote-docker/core/workspace"
)

// go-nfs stats a file it has just created by the absolute OS path osfs returns
// from Create, and every later request by the share-relative one. Both
// spellings must report one fileid, or the CREATE reply carries a number no
// later reply repeats and the Linux client marks the new inode stale: tar
// writes every file and then fails every utime, chown and chmod on it with
// "Stale file handle". Windows was the platform that got it wrong, because
// the absolute path joined onto the root again names nothing and the fileid
// fell back to a hash of the path.
func TestFileIDIsTheSameForAnAbsoluteSpelling(t *testing.T) {
	dir := t.TempDir()
	r := NewRegistry(DefaultAttrs)
	if _, err := r.RegisterCWD(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fresh"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	share, _, ok := r.Lookup(workspace.ExportCWD)
	if !ok {
		t.Fatal("the working directory share is not registered")
	}

	rel, err := share.fs.Lstat("fresh")
	if err != nil {
		t.Fatalf("stat by the relative path: %v", err)
	}
	abs, err := share.fs.Lstat(filepath.Join(dir, "fresh"))
	if err != nil {
		t.Fatalf("stat by the absolute path: %v", err)
	}
	relID := rel.Sys().(*nfsfile.FileInfo).Fileid
	absID := abs.Sys().(*nfsfile.FileInfo).Fileid
	if relID != absID {
		t.Errorf("one file reported two fileids: %#x by its share path, %#x by its absolute path", relID, absID)
	}
}
