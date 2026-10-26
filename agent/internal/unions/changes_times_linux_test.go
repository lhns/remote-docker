//go:build linux

package unions

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/lhns/remote-docker/core/cache"
)

// A client that asked for times is told each entry's ctime: of a file, of a
// whiteout for a deletion, of the marker for a replaced directory. Last writer
// wins reads it, because a container can set an mtime and never a ctime (ADR
// 0044). An empty directory is reported too, and none of it reaches a client
// that did not ask.
func TestChangesCarryTheCtimeOfEachKindWhenAsked(t *testing.T) {
	upper := t.TempDir()
	files := map[string]os.FileMode{
		"made.sh":            0o755,
		"tools/.wh..wh..opq": 0o644,
		"tools/new":          0o644,
		"dir/.wh.gone":       0o644,
	}
	for name, mode := range files {
		p := filepath.Join(upper, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(upper, "emptydir"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A whiteout device needs CAP_MKNOD; without it the marker files above
	// stand for the deletion.
	device := syscall.Mknod(filepath.Join(upper, "dir", "devgone"), syscall.S_IFCHR, 0) == nil

	ctime := func(rel string) int64 {
		info, err := os.Lstat(filepath.Join(upper, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		return ctimeOf(info)
	}

	changes, err := changesIn(&live{}, upper, "/m/aaaa", true, true)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]cache.Change{}
	for _, ch := range changes {
		byPath[ch.Path] = ch
	}
	for path, want := range map[string]int64{
		"/made.sh":   ctime("made.sh"),
		"/tools":     ctime("tools/.wh..wh..opq"),
		"/tools/new": ctime("tools/new"),
		"/dir/gone":  ctime("dir/.wh.gone"),
		"/emptydir":  ctime("emptydir"),
	} {
		ch, ok := byPath[path]
		if !ok {
			t.Errorf("%s was not reported; changes = %+v", path, changes)
			continue
		}
		if want == 0 || ch.Changed != want {
			t.Errorf("%s Changed = %d, want its ctime %d", path, ch.Changed, want)
		}
	}
	if device {
		if ch := byPath["/dir/devgone"]; !ch.Deleted || ch.Changed != ctime("dir/devgone") {
			t.Errorf("a whiteout device reported as %+v, want a deletion with its ctime", ch)
		}
	}
	if ch := byPath["/made.sh"]; ch.Mode != 0o755 {
		t.Errorf("made.sh Mode = %o, want 755", ch.Mode)
	}
	if ch := byPath["/emptydir"]; !ch.Dir {
		t.Errorf("an empty directory reported as %+v, want Dir", ch)
	}
	if _, ok := byPath["/dir"]; ok {
		t.Errorf("a directory holding a whiteout was reported as one the container made: %+v", byPath["/dir"])
	}

	// A client that did not ask is told nothing new.
	plain, err := changesIn(&live{}, upper, "/m/aaaa", true, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range plain {
		if ch.Changed != 0 || ch.Mode != 0 || ch.Dir {
			t.Errorf("a client that did not ask for times was told %+v", ch)
		}
	}
}
