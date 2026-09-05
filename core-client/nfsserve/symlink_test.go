package nfsserve

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// A share must contain what is written through it, symlinks included.
//
// go-billy's BoundOS validates the PARENT directory of a path and evaluates
// symlinks only there, and its Symlink validates where the link is placed and
// never where it points. So the workspace can create a link inside a share
// aimed at any absolute path, and every later operation follows it.
//
// Whether that escapes is the question this answers. It is bounded either way
// by the client's own uid -- the file server runs as the user, so it reaches
// nothing the user could not reach themselves -- but "the export is confined to
// the shares" is a claim the threat model makes, and it should be measured
// rather than assumed.
func TestASymlinkCannotEscapeAShare(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Creating one needs a privilege the tests do not have, so a refusal
		// here would say nothing about containment.
		t.Skip("symlink creation is privileged on this platform")
	}

	base := t.TempDir()
	share := filepath.Join(base, "share")
	if err := os.Mkdir(share, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside.txt")
	if err := os.WriteFile(outside, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := NewRegistry(DefaultAttrs)
	if _, err := r.RegisterCWD(share); err != nil {
		t.Fatal(err)
	}
	target := mustMount(t, serve(t, r), "/cwd")

	// A refusal at the open or the write is containment (billy resolves the
	// link inside the root, where the target does not exist) and is logged.
	// What must not happen is measuring nothing: the link has to exist and
	// the write through it has to be attempted.
	attempted := false
	if err := target.Symlink(outside, "escape"); err != nil {
		t.Logf("the export refused to create the link: %v", err)
	} else if f, err := target.OpenFile("escape", 0o644); err != nil {
		attempted = true
		t.Logf("the export refused to open through the link: %v", err)
	} else {
		attempted = true
		_, werr := f.Write([]byte("ESCAPED"))
		f.Close()
		if werr != nil {
			t.Logf("the write through the link failed: %v", werr)
		}
	}

	got, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) == "ESCAPED" {
		t.Errorf("a file outside the share was written through a link the "+
			"workspace created: %s now holds %q", outside, got)
	}
	if !attempted {
		t.Error("the write through the link was never attempted, so nothing was measured")
	}
}
