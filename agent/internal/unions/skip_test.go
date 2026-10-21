package unions

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func batchOf(t *testing.T, names ...string) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, n := range names {
		if err := tw.WriteHeader(&tar.Header{Name: n, Typeflag: tar.TypeReg, Mode: 0o644, Size: 2, ModTime: time.Now()}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte("ok"))
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &b
}

// A client that asked (cache.Request.Carry) gets the rest of the batch and the
// name of what did not land; one that did not gets the failure it always did,
// because it would read a skipped file as an applied one.
func TestAnEntryTheShareCannotTakeIsSkippedOnlyWhenAsked(t *testing.T) {
	// A name that leaves the share is refused whole by within, which is the
	// portable stand-in for a path the filesystem refuses.
	names := []string{"a", "../escape", "b"}

	root := t.TempDir()
	var landed []string
	skipped, err := extractWith(root, batchOf(t, names...), true, func(name string, _ os.FileInfo) { landed = append(landed, name) })
	if err != nil {
		t.Fatalf("extractWith: %v", err)
	}
	if !slices.Equal(skipped, []string{"/../escape"}) {
		t.Errorf("skipped = %v, want the one entry that could not be written", skipped)
	}
	if !slices.Equal(landed, []string{"/a", "/b"}) {
		t.Errorf("landed = %v, want the entries either side of it", landed)
	}
	for _, f := range []string{"a", "b"} {
		if _, err := os.Stat(filepath.Join(root, f)); err != nil {
			t.Errorf("%s did not land: %v", f, err)
		}
	}

	root = t.TempDir()
	if _, err := extractWith(root, batchOf(t, names...), false, func(string, os.FileInfo) {}); err == nil {
		t.Error("without Carry, an entry the share cannot take did not fail the batch")
	}
}
