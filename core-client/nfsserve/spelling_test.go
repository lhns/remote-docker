package nfsserve

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/go-git/go-billy/v5"
	nfsfile "github.com/willscott/go-nfs/file"

	"github.com/lhns/remote-docker/core/workspace"
)

// One file, every spelling go-nfs might reach it by, one fileid.
//
// go-nfs joins names itself: a lookup joins the handle's path and the name, a
// listing joins the directory and each entry, CREATE stats the absolute path
// osfs returns, and a chroot makes everything relative to somewhere else. If any
// of those spellings produced a different fileid the Linux client would take
// the file for replaced and mark its inode stale, with nothing wrong on the
// server (ADR 0033). A failure here names the spelling that disagreed.
func TestFileIDIsOneWhateverTheSpelling(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a", "b", "c.txt"), []byte("c"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := NewRegistry(DefaultAttrs)
	if _, err := r.RegisterCWD(dir); err != nil {
		t.Fatal(err)
	}
	share, _, ok := r.Lookup(workspace.ExportCWD)
	if !ok {
		t.Fatal("the working directory share is not registered")
	}

	// What the wire says, so the table is anchored to what a client sees and
	// not only to the filesystem being consistent with itself.
	target := mustMount(t, serve(t, r), "/cwd")
	wire, err := target.Getattr("a/b/c.txt")
	if err != nil {
		t.Fatalf("Getattr over the wire: %v", err)
	}
	want := wire.Fileid

	abs := filepath.Join(dir, "a", "b", "c.txt")
	spellings := []spelling{
		{share.fs, "a/b/c.txt", "plain"},
		{share.fs, "./a/b/c.txt", "leading dot"},
		{share.fs, "a//b/c.txt", "doubled separator"},
		{share.fs, "a/./b/c.txt", "dot inside"},
		{share.fs, "a/x/../b/c.txt", "dot-dot through a directory that does not exist"},
		{share.fs, abs, "absolute OS path, as go-nfs stats a fresh CREATE"},
	}
	if runtime.GOOS == "windows" {
		mixed := filepath.ToSlash(filepath.Dir(dir)) + `\` + filepath.Base(dir) + `/a\b/c.txt`
		spellings = append(spellings,
			spelling{share.fs, `a\b\c.txt`, "backslashes"},
			spelling{share.fs, mixed, "absolute path with mixed separators"})

		// A bind written on Windows arrives with forward slashes, and the
		// registry keeps the spelling it was given (Share.LocalPath).
		slashed := NewRegistry(DefaultAttrs)
		if _, err := slashed.RegisterCWD(filepath.ToSlash(dir)); err != nil {
			t.Fatal(err)
		}
		s2, _, ok := slashed.Lookup(workspace.ExportCWD)
		if !ok {
			t.Fatal("the forward-slash share is not registered")
		}
		spellings = append(spellings, spelling{s2.fs, "a/b/c.txt", "root registered with forward slashes"})
	}
	sub, err := share.fs.Chroot("a")
	if err != nil {
		t.Fatalf("Chroot: %v", err)
	}
	spellings = append(spellings, spelling{sub, "b/c.txt", "through Chroot(a), as a subdirectory mount sees it"})

	for _, sp := range spellings {
		t.Run(sp.why, func(t *testing.T) {
			if fi, err := sp.fs.Lstat(sp.name); err != nil {
				t.Errorf("Lstat(%q): %v", sp.name, err)
			} else if got := fileidOf(fi); got != want {
				t.Errorf("Lstat(%q) reports fileid %#x, the wire says %#x", sp.name, got, want)
			}
			if fi, err := sp.fs.Stat(sp.name); err != nil {
				t.Errorf("Stat(%q): %v", sp.name, err)
			} else if got := fileidOf(fi); got != want {
				t.Errorf("Stat(%q) reports fileid %#x, the wire says %#x", sp.name, got, want)
			}

			// A listing is asked for the directory in the same spelling, and
			// the entry is what it joins to it.
			parent, base := splitAnySeparator(sp.name)
			entries, err := sp.fs.ReadDir(parent)
			if err != nil {
				t.Fatalf("ReadDir(%q): %v", parent, err)
			}
			found := false
			for _, e := range entries {
				if e.Name() != base {
					continue
				}
				found = true
				if got := fileidOf(e); got != want {
					t.Errorf("ReadDir(%q) entry %q reports fileid %#x, the wire says %#x", parent, base, got, want)
				}
			}
			if !found {
				t.Errorf("ReadDir(%q) did not list %q", parent, base)
			}
		})
	}
}

// spelling is one way of naming a/b/c.txt on one filesystem.
type spelling struct {
	fs   billy.Filesystem
	name string
	why  string
}

// fileidOf is what the wire would carry for a FileInfo the share returned.
func fileidOf(fi os.FileInfo) uint64 {
	return fi.Sys().(*nfsfile.FileInfo).Fileid
}

// splitAnySeparator splits on the last slash of either kind, since a spelling
// under test may mix them.
func splitAnySeparator(name string) (dir, base string) {
	i := strings.LastIndexAny(name, `/\`)
	if i < 0 {
		return ".", name
	}
	return name[:i], name[i+1:]
}

// "" and "." both name the share itself, and go-nfs asks with both: the
// mount handle's path is empty, and a lookup of "." is answered from it.
func TestFileIDOfTheRootIsOneSpelling(t *testing.T) {
	dir := t.TempDir()
	r := NewRegistry(DefaultAttrs)
	if _, err := r.RegisterCWD(dir); err != nil {
		t.Fatal(err)
	}
	share, _, _ := r.Lookup(workspace.ExportCWD)
	empty, err := share.fs.Stat("")
	if err != nil {
		t.Fatalf(`Stat(""): %v`, err)
	}
	for _, name := range []string{".", "./"} {
		fi, err := share.fs.Stat(name)
		if err != nil {
			t.Fatalf("Stat(%q): %v", name, err)
		}
		if got, want := fileidOf(fi), fileidOf(empty); got != want {
			t.Errorf("the root has fileid %#x as %q and %#x as \"\"", got, name, want)
		}
	}
}
