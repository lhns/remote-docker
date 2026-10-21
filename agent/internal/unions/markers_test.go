package unions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// H3. fuse-overlayfs may record a deletion and an opaque directory as files
// named .wh.<name> and .wh..wh..opq instead of a device and an xattr. Reported
// as the container's files, they were written into the user's tree, and the
// deletions they stand for never arrived (ADR 0044).
func TestAnOverlayMarkerFileIsNeverReportedAsTheContainersFile(t *testing.T) {
	upper := t.TempDir()
	for name, body := range map[string]string{
		"tools/.wh..wh..opq": "",
		"tools/.wh..opq":     "",
		"tools/new":          "made after the directory was replaced",
		"dir/.wh.foo":        "",
		"dir/keep":           "kept",
	} {
		p := filepath.Join(upper, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	for _, carry := range []bool{false, true} {
		changes, err := changesIn(&live{}, upper, "/m/aaaa", carry)
		if err != nil {
			t.Fatalf("changesIn: %v", err)
		}

		deleted, opaque := map[string]bool{}, map[string]bool{}
		for _, ch := range changes {
			if strings.Contains(ch.Path, ".wh.") {
				t.Errorf("carry=%v: an overlay marker was reported as a file: %+v", carry, ch)
			}
			if ch.Deleted {
				deleted[ch.Path] = true
			}
			if ch.Opaque {
				opaque[ch.Path] = true
			}
		}
		if !deleted["/dir/foo"] {
			t.Errorf("carry=%v: the .wh.foo marker did not report /dir/foo as deleted; changes = %+v", carry, changes)
		}
		// Told only to a client that asked: one that did not would read a
		// directory as an empty file.
		if opaque["/tools"] != carry {
			t.Errorf("carry=%v: opaque directories reported = %v; changes = %+v", carry, opaque, changes)
		}
		var sawNew, sawKeep bool
		for _, ch := range changes {
			sawNew = sawNew || ch.Path == "/tools/new"
			sawKeep = sawKeep || ch.Path == "/dir/keep"
		}
		if !sawNew || !sawKeep {
			t.Errorf("carry=%v: an ordinary file was lost beside the markers; changes = %+v", carry, changes)
		}
	}
}

// A link the container made is reported with its target, verbatim, and only to
// a client that asked; it is never followed.
func TestALinkIsReportedWithItsTargetOnlyWhenAsked(t *testing.T) {
	upper, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(upper, "out")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../tool/bin", filepath.Join(upper, "rel")); err != nil {
		t.Fatal(err)
	}

	changes, err := changesIn(&live{}, upper, "/m/aaaa", false)
	if err != nil || len(changes) != 0 {
		t.Errorf("a client that did not ask was told %+v (%v)", changes, err)
	}

	changes, err = changesIn(&live{}, upper, "/m/aaaa", true)
	if err != nil {
		t.Fatal(err)
	}
	links := map[string]string{}
	for _, ch := range changes {
		if strings.Contains(ch.Path, "secret") {
			t.Errorf("the walk followed a link out of the layer: %+v", ch)
		}
		links[ch.Path] = ch.Link
	}
	if links["/out"] != outside || links["/rel"] != "../tool/bin" {
		t.Errorf("links = %v, want both targets verbatim", links)
	}
}
