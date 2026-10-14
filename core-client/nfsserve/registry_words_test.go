package nfsserve

import (
	"os"
	"path/filepath"
	"testing"

	nfsfile "github.com/willscott/go-nfs/file"

	"github.com/lhns/remote-docker/core/workspace"
)

// ownerOf is the uid and gid a share reports for its root, as the wire sees
// them.
func ownerOf(t *testing.T, s *Share) (uint32, uint32) {
	t.Helper()
	fi, err := s.fs.Stat("/")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	wire, ok := fi.Sys().(*nfsfile.FileInfo)
	if !ok {
		t.Fatalf("Sys() = %T, want the NFS file info", fi.Sys())
	}
	return wire.UID, wire.GID
}

// The owner is a property of the share: one reports what it was registered
// with, the next the registry's default, and the registry's other attributes
// (AlwaysExecutable, which Windows cannot do without) are not lost to the
// override.
func TestRegisterOwnerIsPerShare(t *testing.T) {
	r := NewRegistry(Attrs{UID: 7, GID: 8, FileMode: 0o666, DirMode: 0o777, AlwaysExecutable: true})

	named := t.TempDir()
	if err := os.WriteFile(filepath.Join(named, "run.sh"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := r.Register(named, workspace.ShareWords{Owner: workspace.Owner{UID: 1000, GID: 2000, Set: true}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.Register(t.TempDir(), workspace.ShareWords{})
	if err != nil {
		t.Fatal(err)
	}

	if uid, gid := ownerOf(t, a); uid != 1000 || gid != 2000 {
		t.Errorf("named share reports %d:%d, want 1000:2000", uid, gid)
	}
	if uid, gid := ownerOf(t, b); uid != 7 || gid != 8 {
		t.Errorf("unnamed share reports %d:%d, want the registry's 7:8", uid, gid)
	}

	// Words are the resolved ones, so shares compare without knowing where a
	// default came from.
	if a.Words.Owner != (workspace.Owner{UID: 1000, GID: 2000, Set: true}) || b.Words.Owner != (workspace.Owner{UID: 7, GID: 8, Set: true}) {
		t.Errorf("Words = %v and %v", a.Words, b.Words)
	}

	fi, err := a.fs.Stat("/run.sh")
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&0o111 == 0 {
		t.Errorf("run.sh mode = %v; the override dropped AlwaysExecutable", fi.Mode())
	}
}

// A directory already registered comes back with the words it was first
// registered with: attributes are fixed for the life of the share, because
// replacing a filesystem under a live handle strands it.
func TestRegisterKeepsTheFirstWords(t *testing.T) {
	r := NewRegistry(Attrs{})
	dir := t.TempDir()

	first, err := r.Register(dir, workspace.ShareWords{Owner: workspace.Owner{UID: 1000, GID: 1000, Set: true}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.Register(dir, workspace.ShareWords{Owner: workspace.Owner{UID: 2000, GID: 2000, Set: true}})
	if err != nil {
		t.Fatal(err)
	}
	if second != first || second.Words.Owner.UID != 1000 {
		t.Errorf("re-registering returned %v, want the first share unchanged", second.Words)
	}
	if uid, _ := ownerOf(t, second); uid != 1000 {
		t.Errorf("share reports uid %d after a second registration, want 1000", uid)
	}
}

// A restored share is registered with the words the record held.
func TestRestoreCarriesTheWords(t *testing.T) {
	dir := t.TempDir()
	export := workspace.ExportPathForID(workspace.ShareID(dir))

	r := NewRegistry(Attrs{})
	r.Restore = func(path string) (string, workspace.ShareWords, bool) {
		return dir, workspace.ShareWords{Owner: workspace.Owner{UID: 1000, GID: 1000, Set: true}}, path == export
	}
	share, _, ok := r.LookupOrRestore(export)
	if !ok {
		t.Fatal("not restored")
	}
	if uid, _ := ownerOf(t, share); uid != 1000 {
		t.Errorf("restored share reports uid %d, want the recorded 1000", uid)
	}
}
