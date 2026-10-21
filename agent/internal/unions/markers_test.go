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

	changes, err := changesIn(&live{}, upper, "/m/aaaa")
	if err != nil {
		t.Fatalf("changesIn: %v", err)
	}

	deleted := map[string]bool{}
	for _, ch := range changes {
		if strings.Contains(ch.Path, ".wh.") {
			t.Errorf("an overlay marker was reported as a file: %+v", ch)
		}
		if ch.Deleted {
			deleted[ch.Path] = true
		}
	}
	if !deleted["/dir/foo"] {
		t.Errorf("the .wh.foo marker did not report /dir/foo as deleted; changes = %+v", changes)
	}
	var sawNew, sawKeep bool
	for _, ch := range changes {
		sawNew = sawNew || ch.Path == "/tools/new"
		sawKeep = sawKeep || ch.Path == "/dir/keep"
	}
	if !sawNew || !sawKeep {
		t.Errorf("an ordinary file was lost beside the markers; changes = %+v", changes)
	}
}
