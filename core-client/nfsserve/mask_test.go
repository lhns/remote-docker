package nfsserve

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lhns/remote-docker/core/workspace"
)

// stubInfo is a FileInfo with the mode a test names, so a real execute bit can
// be had on a platform whose filesystem has none.
type stubInfo struct{ mode fs.FileMode }

func (s stubInfo) Name() string       { return "f" }
func (s stubInfo) Size() int64        { return 0 }
func (s stubInfo) Mode() fs.FileMode  { return s.mode }
func (s stubInfo) ModTime() time.Time { return time.Time{} }
func (s stubInfo) IsDir() bool        { return s.mode.IsDir() }
func (s stubInfo) Sys() any           { return nil }

// The mask is cleared AFTER the execute bit is decided, so fmask=0111 takes
// away an execute bit whether Windows invented it or the file really has one.
func TestMasksClearReportedBits(t *testing.T) {
	base := Attrs{FileMode: 0o666, DirMode: 0o777}
	for name, tc := range map[string]struct {
		attrs Attrs
		real  fs.FileMode
		want  fs.FileMode
	}{
		"no mask, from Windows":       {withExec(base, 0, 0), 0o666, 0o777},
		"fmask 0111, from Windows":    {withExec(base, 0o111, 0), 0o666, 0o666},
		"fmask 0111, a real exec bit": {withMasks(base, 0o111, 0), 0o755, 0o666},
		"fmask 022, a real exec bit":  {withMasks(base, 0o022, 0), 0o755, 0o755},
		"no mask, a directory":        {base, fs.ModeDir | 0o755, fs.ModeDir | 0o777},
		"dmask 022, a directory":      {withMasks(base, 0, 0o022), fs.ModeDir | 0o700, fs.ModeDir | 0o755},
		"fmask leaves a directory":    {withMasks(base, 0o111, 0), fs.ModeDir | 0o700, fs.ModeDir | 0o777},
		"dmask leaves a file":         {withMasks(base, 0, 0o022), 0o644, 0o666},
		"type bits survive":           {withMasks(base, 0o111, 0), fs.ModeSymlink | 0o777, fs.ModeSymlink | 0o666},
	} {
		got := (&attrInfo{FileInfo: stubInfo{tc.real}, attrs: tc.attrs}).Mode()
		if got != tc.want {
			t.Errorf("%s: mode = %v, want %v", name, got, tc.want)
		}
	}
}

func withMasks(a Attrs, file, dir fs.FileMode) Attrs {
	a.FileMask, a.DirMask = file, dir
	return a
}

func withExec(a Attrs, file, dir fs.FileMode) Attrs {
	a = withMasks(a, file, dir)
	a.AlwaysExecutable = true
	return a
}

// A mask is part of the share, like the owner: the same directory with
// another mask is another share reporting its own modes, and one naming none
// takes the registry's.
func TestMasksArePerShare(t *testing.T) {
	r := NewRegistry(Attrs{FileMode: 0o666, DirMode: 0o777, AlwaysExecutable: true, DirMask: 0o002})
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fmask := workspace.Mask{Bits: 0o111, Set: true}

	plain, err := r.Register(dir, workspace.DefaultMode, workspace.ShareWords{})
	if err != nil {
		t.Fatal(err)
	}
	masked, err := r.Register(dir, workspace.DefaultMode, workspace.ShareWords{FMask: fmask, DMask: workspace.Mask{Bits: 0o022, Set: true}})
	if err != nil {
		t.Fatal(err)
	}
	if plain == masked || plain.ExportPath == masked.ExportPath {
		t.Fatalf("two masks of one directory are one share at %s", plain.ExportPath)
	}

	for _, tc := range []struct {
		share      *Share
		file, root fs.FileMode
	}{
		{plain, 0o777, 0o775},
		{masked, 0o666, 0o755},
	} {
		if got := modeOf(t, tc.share, "/f"); got != tc.file {
			t.Errorf("%s: file reports %04o, want %04o", tc.share.ExportPath, got, tc.file)
		}
		if got := modeOf(t, tc.share, "/"); got != tc.root {
			t.Errorf("%s: directory reports %04o, want %04o", tc.share.ExportPath, got, tc.root)
		}
	}

	// Resolved, so a record and a later mount compare without knowing where a
	// default came from.
	if w := plain.Words(); w.FMask != (workspace.Mask{Set: true}) || w.DMask != (workspace.Mask{Bits: 0o002, Set: true}) {
		t.Errorf("unnamed masks resolve to %v, %v", w.FMask, w.DMask)
	}
	again, err := r.Register(dir, workspace.DefaultMode, workspace.ShareWords{FMask: fmask, DMask: workspace.Mask{Bits: 0o022, Set: true}})
	if err != nil || again != masked {
		t.Errorf("the same masks made another share: %v", err)
	}
}

func modeOf(t *testing.T, s *Share, name string) fs.FileMode {
	t.Helper()
	fi, err := s.fs.Stat(name)
	if err != nil {
		t.Fatalf("Stat(%s): %v", name, err)
	}
	return fi.Mode() & fs.ModePerm
}

// A recorded masked share comes back under its masks; a record naming none
// comes back unmasked, as its id was derived, even where the registry's
// default masks.
func TestRestoreUnderTheRecordedMasks(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fmask := workspace.Mask{Bits: 0o111, Set: true}
	masked := workspace.ExportPathForID(workspace.ShareIDFor(dir, workspace.ShareVariant{Mode: cachedBack, Owner: owner1000, FMask: fmask}))
	plain := workspace.ExportPathForID(workspace.ShareIDFor(dir, workspace.ShareVariant{Mode: cachedBack, Owner: owner1000}))
	if masked == plain {
		t.Fatal("a mask did not change the export")
	}

	r := NewRegistry(Attrs{FileMode: 0o666, DirMode: 0o777, AlwaysExecutable: true, FileMask: 0o111})
	r.Restore = func(path string) (string, workspace.Mode, workspace.ShareWords, bool) {
		switch path {
		case masked:
			return dir, cachedBack, workspace.ShareWords{Owner: owner1000, FMask: fmask}, true
		case plain:
			return dir, cachedBack, workspace.ShareWords{Owner: owner1000}, true
		}
		return "", workspace.ModeUnset, workspace.ShareWords{}, false
	}

	for export, want := range map[string]fs.FileMode{masked: 0o666, plain: 0o777} {
		share, _, ok := r.LookupOrRestore(export)
		if !ok {
			t.Fatalf("%s was not restored", export)
		}
		if got := modeOf(t, share, "/f"); got != want {
			t.Errorf("%s: restored file reports %04o, want %04o", export, got, want)
		}
	}

	// A record naming a mask under the export of another is refused.
	other := NewRegistry(Attrs{})
	other.Restore = func(string) (string, workspace.Mode, workspace.ShareWords, bool) {
		return dir, cachedBack, workspace.ShareWords{Owner: owner1000, FMask: workspace.Mask{Bits: 0o022, Set: true}}, true
	}
	if _, _, ok := other.LookupOrRestore(masked); ok {
		t.Error("restored an export its record's masks do not derive")
	}
}
