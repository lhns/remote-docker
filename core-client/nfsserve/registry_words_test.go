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
	a, err := r.Register(named, workspace.ModeUnset, workspace.ShareWords{Owner: workspace.Owner{UID: 1000, GID: 2000, Set: true}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.Register(t.TempDir(), workspace.ModeUnset, workspace.ShareWords{})
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
	if a.Words().Owner != (workspace.Owner{UID: 1000, GID: 2000, Set: true}) || b.Words().Owner != (workspace.Owner{UID: 7, GID: 8, Set: true}) {
		t.Errorf("Words = %v and %v", a.Words(), b.Words())
	}

	fi, err := a.fs.Stat("/run.sh")
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&0o111 == 0 {
		t.Errorf("run.sh mode = %v; the override dropped AlwaysExecutable", fi.Mode())
	}
}

var (
	cachedBack = workspace.Mode{Read: workspace.ReadCached, Write: workspace.WriteBack}
	owner1000  = workspace.Owner{UID: 1000, GID: 1000, Set: true}
	owner2000  = workspace.Owner{UID: 2000, GID: 2000, Set: true}
)

// One directory mounted two ways is two shares: own export, own root handle,
// both resolving, each with the owner it was registered with.
func TestVariantsOfOneDirectoryAreTwoShares(t *testing.T) {
	r := NewRegistry(Attrs{})
	dir := t.TempDir()

	a, err := r.Register(dir, workspace.DefaultMode, workspace.ShareWords{Owner: owner1000})
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.Register(dir, workspace.DefaultMode, workspace.ShareWords{Owner: owner2000})
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.Register(dir, cachedBack, workspace.ShareWords{Owner: owner1000})
	if err != nil {
		t.Fatal(err)
	}

	exports := map[string]bool{a.ExportPath: true, b.ExportPath: true, c.ExportPath: true}
	if a == b || a == c || b == c || len(exports) != 3 {
		t.Fatalf("variants share a share: exports %v", exports)
	}
	if uid, _ := ownerOf(t, a); uid != 1000 {
		t.Errorf("first variant reports uid %d, want 1000", uid)
	}
	if uid, _ := ownerOf(t, b); uid != 2000 {
		t.Errorf("second variant reports uid %d, want 2000", uid)
	}

	s := New(r, nil)
	handles := map[string]bool{}
	for _, share := range []*Share{a, b, c} {
		if got, _, ok := r.Lookup(share.ExportPath); !ok || got != share {
			t.Errorf("%s does not resolve to its own share", share.ExportPath)
		}
		handles[string(rootHandleOf(t, s, share.ExportPath))] = true
	}
	if len(handles) != 3 {
		t.Errorf("three variants were given %d distinct root handles", len(handles))
	}
	if n := len(r.Shares()); n != 3 {
		t.Errorf("Shares reports %d, want 3", n)
	}
}

// The same variant is one share, and a later mount can only raise its watch and
// prefetch: the owner is identity and the filesystem is built once.
func TestRegisterRaisesWatchAndPrefetch(t *testing.T) {
	r := NewRegistry(Attrs{})
	dir := t.TempDir()

	first, err := r.Register(dir, cachedBack, workspace.ShareWords{Owner: owner1000, Watch: workspace.WatchPartial, Prefetch: workspace.PrefetchEager})
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.Register(dir, cachedBack, workspace.ShareWords{Owner: owner1000, Watch: workspace.WatchCoarse, Prefetch: workspace.PrefetchTree})
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatal("the same variant registered twice gave two shares")
	}
	if got := first.Words(); got.Watch != workspace.WatchCoarse || got.Prefetch != workspace.PrefetchEager {
		t.Errorf("words = %+v, want coarse (raised) and eager (kept)", got)
	}

	if _, err := r.Register(dir, cachedBack, workspace.ShareWords{Owner: owner1000, Watch: workspace.WatchOff}); err != nil {
		t.Fatal(err)
	}
	if got := first.Words().Watch; got != workspace.WatchCoarse {
		t.Errorf("a weaker mount lowered watch to %q", got)
	}
	if uid, _ := ownerOf(t, first); uid != 1000 {
		t.Errorf("share reports uid %d, want 1000", uid)
	}
}

// Words are read while mounts raise them, and the share pointer is held by the
// watcher and the cache. Run under -race.
func TestRaiseIsSafeAgainstReaders(t *testing.T) {
	r := NewRegistry(Attrs{})
	dir := t.TempDir()
	share, err := r.Register(dir, workspace.DefaultMode, workspace.ShareWords{})
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 200 {
			_ = share.Words()
		}
	}()
	for range 200 {
		if _, err := r.Register(dir, workspace.DefaultMode, workspace.ShareWords{Watch: workspace.WatchCoarse}); err != nil {
			t.Error(err)
		}
	}
	<-done
}

// A restored share is registered with the words the record held.
func TestRestoreCarriesTheWords(t *testing.T) {
	dir := t.TempDir()
	export := workspace.ExportPathForID(workspace.LegacyShareID(dir))

	r := NewRegistry(Attrs{})
	r.Restore = func(path string) (string, workspace.Mode, workspace.ShareWords, bool) {
		return dir, workspace.ModeUnset, workspace.ShareWords{Owner: workspace.Owner{UID: 1000, GID: 1000, Set: true}}, path == export
	}
	var told []*Share
	r.Restored = func(s *Share) { told = append(told, s) }

	share, _, ok := r.LookupOrRestore(export)
	if !ok {
		t.Fatal("not restored")
	}
	if uid, _ := ownerOf(t, share); uid != 1000 {
		t.Errorf("restored share reports uid %d, want the recorded 1000", uid)
	}
	if len(told) != 1 || told[0] != share {
		t.Errorf("Restored was called with %v, want the restored share once", told)
	}

	// A share that is already there is not a restore.
	if _, _, ok := r.LookupOrRestore(export); !ok || len(told) != 1 {
		t.Errorf("a lookup of a registered share called Restored again: %d calls", len(told))
	}
}

// A record that names a variant restores under the export it recorded, and a
// new mount of the same variant finds it rather than making another.
func TestRestoreUnderTheRecordedVariant(t *testing.T) {
	dir := t.TempDir()
	variant := workspace.ShareVariant{Mode: cachedBack, Owner: owner1000}
	export := workspace.ExportPathForID(workspace.ShareIDFor(dir, variant))

	r := NewRegistry(Attrs{})
	r.Restore = func(path string) (string, workspace.Mode, workspace.ShareWords, bool) {
		return dir, cachedBack, workspace.ShareWords{Owner: owner1000, Watch: workspace.WatchPartial}, path == export
	}

	restored, _, ok := r.LookupOrRestore(export)
	if !ok {
		t.Fatal("a recorded variant was not restored")
	}
	if restored.Mode != cachedBack {
		t.Errorf("restored mode = %v, want the recorded one", restored.Mode)
	}
	again, err := r.Register(dir, cachedBack, workspace.ShareWords{Owner: owner1000})
	if err != nil {
		t.Fatal(err)
	}
	if again != restored {
		t.Error("a mount of the recorded variant made another share")
	}
	if got := again.Words().Watch; got != workspace.WatchPartial {
		t.Errorf("watch = %q, want the recorded one kept", got)
	}
}

// A record whose export is not what its path and variant derive is refused.
func TestRestoreRefusesAnExportThatDoesNotMatch(t *testing.T) {
	dir := t.TempDir()
	legacy := workspace.ExportPathForID(workspace.LegacyShareID(dir))
	variantExport := workspace.ExportPathForID(workspace.ShareIDFor(dir, workspace.ShareVariant{Mode: cachedBack, Owner: owner1000}))

	for name, tc := range map[string]struct {
		export string
		mode   workspace.Mode
	}{
		"a variant record under the legacy export": {legacy, cachedBack},
		"a legacy record under a variant export":   {variantExport, workspace.ModeUnset},
		"a variant record under another mode":      {variantExport, workspace.DefaultMode},
		"an export for another directory":          {"/m/0123456789abcdef", workspace.ModeUnset},
	} {
		t.Run(name, func(t *testing.T) {
			r := NewRegistry(Attrs{})
			r.Restore = func(string) (string, workspace.Mode, workspace.ShareWords, bool) {
				return dir, tc.mode, workspace.ShareWords{Owner: owner1000}, true
			}
			if _, _, ok := r.LookupOrRestore(tc.export); ok {
				t.Error("restored an export its record does not derive")
			}
		})
	}
}

// A share restored from a record that predates the variant keeps serving the
// containers mounting it, but is not what a new mount of the directory
// resolves to: that mount gets a share, export and volume of its own.
func TestALegacyShareIsNeverReusedByANewMount(t *testing.T) {
	dir := t.TempDir()
	legacy := workspace.ExportPathForID(workspace.LegacyShareID(dir))

	r := NewRegistry(Attrs{})
	r.Restore = func(path string) (string, workspace.Mode, workspace.ShareWords, bool) {
		return dir, workspace.ModeUnset, workspace.ShareWords{Owner: owner1000}, path == legacy
	}
	old, _, ok := r.LookupOrRestore(legacy)
	if !ok {
		t.Fatal("the legacy record was not restored")
	}
	if old.ExportPath != legacy {
		t.Fatalf("restored under %q, want the recorded %q", old.ExportPath, legacy)
	}

	fresh, err := r.Register(dir, workspace.DefaultMode, workspace.ShareWords{Owner: owner1000})
	if err != nil {
		t.Fatal(err)
	}
	if fresh == old || fresh.ExportPath == legacy {
		t.Errorf("a new mount reused the legacy share at %s", fresh.ExportPath)
	}
	if _, _, ok := r.Lookup(legacy); !ok {
		t.Error("the legacy share stopped resolving")
	}
}
