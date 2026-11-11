package unions

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// A pull is spooled to a file and copied by the size each header declares, so
// a large file written by a container is never held in the agent's memory.
func TestPullOfALargeFileDoesNotBufferIt(t *testing.T) {
	const size = 96 << 20
	upper := t.TempDir()
	f, err := os.Create(filepath.Join(upper, "big"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil { // sparse where the filesystem allows
		t.Fatal(err)
	}
	_ = f.Close()

	for _, carry := range []bool{false, true} {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)

		spool, _, err := pull(rootAt(t, upper), []string{"/big"}, carry)
		if err != nil {
			t.Fatal(err)
		}
		runtime.ReadMemStats(&after)

		// Allocation over the whole call, not the live heap: a buffer of the
		// file's size would show here whether or not it was collected.
		if grown := after.TotalAlloc - before.TotalAlloc; grown > size/4 {
			t.Errorf("carry=%v: pulling a %d byte file allocated %d bytes", carry, int64(size), grown)
		}

		tr := tar.NewReader(spool)
		h, err := tr.Next()
		if err != nil || h.Name != "big" || h.Size != size {
			t.Fatalf("carry=%v: entry = %+v, %v; want big of %d bytes", carry, h, err, int64(size))
		}
		n, err := io.Copy(io.Discard, tr)
		if err != nil || n != size {
			t.Errorf("carry=%v: read %d bytes, %v", carry, n, err)
		}
		if _, err := tr.Next(); !errors.Is(err, io.EOF) {
			t.Errorf("carry=%v: the archive holds more than big: %v", carry, err)
		}
		name := spool.Name()
		_ = spool.Close()
		if _, err := os.Stat(name); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("carry=%v: the spool file was left behind: %v", carry, err)
		}
	}
}

// Several files, one of them gone, come out as a whole archive in the order
// asked, with nothing for the one that is missing.
func TestPullCarriesSeveralFilesAndSkipsAGoneOne(t *testing.T) {
	upper := t.TempDir()
	want := map[string]string{"a": "one", "d/b": "two"}
	if err := os.MkdirAll(filepath.Join(upper, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range want {
		if err := os.WriteFile(filepath.Join(upper, filepath.FromSlash(name)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out, _, err := pulled(t, rootAt(t, upper), []string{"/a", "/gone", "/d/b"}, true)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(bytes.NewReader(out))
	got := map[string]string{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		got[h.Name] = string(b)
	}
	if len(got) != 2 || got["a"] != "one" || got["d/b"] != "two" {
		t.Errorf("pulled %v, want %v", got, want)
	}
}
