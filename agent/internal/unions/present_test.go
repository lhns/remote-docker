package unions

import (
	"archive/tar"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// Which paths exist is answered from the merged view by lstat, so a file the
// client wrote through the union counts, a deleted one does not, and a link is
// reported as itself and never followed.
func TestPresentAnswersFromTheMergedViewWithoutFollowingLinks(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "kept"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "nowhere"), filepath.Join(root, "dangling")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}

	got, err := present(root, []string{"/kept", "/gone", "/dangling", "/../escape"})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	// A dangling link is there; a path that cannot be asked about is never
	// claimed absent.
	if want := []string{"/../escape", "/dangling", "/kept"}; !slices.Equal(got, want) {
		t.Errorf("present = %v, want %v", got, want)
	}
}

// One path that cannot be named must not fail the chunk and starve the rest.
func TestPullSkipsAPathItCannotNameOnlyWhenAsked(t *testing.T) {
	upper := t.TempDir()
	if err := os.WriteFile(filepath.Join(upper, "good"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	paths := []string{"/../bad", "/good"}

	tarball, skipped, err := pull(upper, paths, true)
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if !slices.Equal(names(skipped), []string{"/../bad"}) {
		t.Errorf("skipped = %v, want the bad path named", skipped)
	}
	h, err := tar.NewReader(bytes.NewReader(tarball)).Next()
	if err != nil || h.Name != "good" {
		t.Errorf("the good file was not carried: %v %v", h, err)
	}

	if _, _, err := pull(upper, paths, false); err == nil {
		t.Error("without Carry a bad path did not fail the request, as it always did")
	}
}

// A file this entry did not create is never removed after a failed write; one it
// did create is.
func TestOnlyAFileTheEntryCreatedIsRemovedAfterAFailedWrite(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "theirs"), []byte("container's"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"theirs", "mine"} {
		var cut bytes.Buffer
		tw := tar.NewWriter(&cut)
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: 4}); err != nil {
			t.Fatal(err)
		}
		cut.WriteString("xx")
		_, _ = extract(root, &cut, true, func(string, os.FileInfo) {})
	}
	if _, err := os.Stat(filepath.Join(root, "theirs")); err != nil {
		t.Errorf("a file the entry did not create was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "mine")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a file the entry created and could not finish was left: %v", err)
	}
}
