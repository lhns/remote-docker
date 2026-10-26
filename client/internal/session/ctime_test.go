package session

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/lhns/remote-docker/core/workspace"
)

// This machine's change time is what write-back's last writer wins reads for a
// file the cache never held (ADR 0044). It must be readable on the platforms a
// client runs on, and it must MOVE when the file is written, which is the whole
// point of not reading the modification time.
func TestChangeTimeIsReadAndMovesOnAWrite(t *testing.T) {
	switch runtime.GOOS {
	case "linux", "darwin", "windows":
	default:
		t.Skipf("no change time is read on %s", runtime.GOOS)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "index.json")
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A time set far in the past, as tar and cp -p do: the change time must
	// not follow it.
	old := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()

	first, ok := changeTime(r, "index.json")
	if !ok {
		t.Fatal("the change time could not be read")
	}
	if first.Before(time.Now().Add(-time.Hour)) {
		t.Errorf("the change time %v followed the modification time set to %v", first, old)
	}

	time.Sleep(20 * time.Millisecond)
	if err := os.WriteFile(path, []byte(`{"a":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	second, ok := changeTime(r, "index.json")
	if !ok || !second.After(first) {
		t.Errorf("after a write the change time is %v (%v), was %v: it must move", second, ok, first)
	}

	if _, ok := changeTime(r, "missing"); ok {
		t.Error("a missing file has a change time")
	}

	// A directory too: a removed directory is taken here only if nothing in it
	// changed after the container removed it, and an unreadable time keeps it.
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	before, ok := changeTime(r, "sub")
	if !ok {
		t.Fatal("a directory's change time could not be read")
	}
	time.Sleep(20 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(dir, "sub", "made"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if after, ok := changeTime(r, "sub"); !ok || !after.After(before) {
		t.Errorf("after a file was made in it, the directory's change time is %v (%v), was %v", after, ok, before)
	}
}

// The execute bits a share's container sees as this machine's file has them:
// none on Windows, and on every other host 0111 less the share's file mask.
func TestExecBits(t *testing.T) {
	for _, c := range []struct {
		goos    string
		mask    workspace.Mask
		session os.FileMode
		want    os.FileMode
	}{
		{"windows", workspace.Mask{}, 0, 0},
		{"linux", workspace.Mask{}, 0, 0o111},
		{"linux", workspace.Mask{}, 0o011, 0o100},
		{"darwin", workspace.Mask{Bits: 0o111, Set: true}, 0, 0},
		{"linux", workspace.Mask{Bits: 0, Set: true}, 0o111, 0o111},
	} {
		if got := execBits(c.goos, c.mask, c.session); got != c.want {
			t.Errorf("execBits(%s, %+v, %o) = %o, want %o", c.goos, c.mask, c.session, got, c.want)
		}
	}
}
