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

// A file this batch could not finish is removed only if the batch created it.
// The root is the merged mount, so removing one that was already there is a
// whiteout: it would hide the live file from the container instead of letting a
// read fall through to it.
func TestASkippedWriteRemovesOnlyWhatItCreated(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "existing"), []byte("live content"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"existing", "created"} {
		// A header promising 4 bytes followed by 2: the copy fails half way.
		var cut bytes.Buffer
		tw := tar.NewWriter(&cut)
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: 4, ModTime: time.Now()}); err != nil {
			t.Fatal(err)
		}
		_, _ = cut.WriteString("xx")

		skipped, _ := extractWith(root, &cut, true, func(string, os.FileInfo) {})
		if !slices.Equal(skipped, []string{"/" + name}) {
			t.Fatalf("%s: skipped = %v, want it named", name, skipped)
		}
	}

	if _, err := os.Stat(filepath.Join(root, "existing")); err != nil {
		t.Errorf("a file that was already there is gone after a failed write: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "created")); err == nil {
		t.Error("a file the failed write created was left behind, short")
	}
}
