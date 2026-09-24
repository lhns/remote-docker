package unions

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A container writes into its union, so a symlink in the share is the
// container's to choose, and the agent follows it as root in its own
// namespace: through /proc/<pid>/root an absolute target resolves against the
// AGENT's root. These are the three ways a request could be steered out.

// share is a share root with a directory outside it, and a symlink in the share
// pointing there. Skipped where symlinks cannot be made (unelevated Windows).
func share(t *testing.T) (root, outside string) {
	t.Helper()
	root, outside = t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "evil")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	return root, outside
}

func TestApplyDoesNotWriteThroughASymlinkOutOfTheShare(t *testing.T) {
	root, outside := share(t)

	var batch bytes.Buffer
	tw := tar.NewWriter(&batch)
	body := []byte("pwned")
	if err := tw.WriteHeader(&tar.Header{Name: "evil/pwned", Typeflag: tar.TypeReg, Mode: 0o644,
		Size: int64(len(body)), ModTime: time.Now()}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write(body)
	_ = tw.Close()

	err := extract(root, &batch, func(string, os.FileInfo) {})
	if _, statErr := os.Stat(filepath.Join(outside, "pwned")); statErr == nil {
		t.Fatal("a fill followed a symlink and wrote outside the share")
	}
	if err == nil {
		t.Error("a write through an escaping symlink was not refused")
	}
}

func TestDropDoesNotRemoveThroughASymlinkOutOfTheShare(t *testing.T) {
	root, outside := share(t)
	victim := filepath.Join(outside, "victim")
	if err := os.WriteFile(victim, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	_ = remove(root, []string{"/evil/victim"}, func(string) {})
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("a drop followed a symlink and removed a file outside the share: %v", err)
	}
}

func TestPullDoesNotReadThroughASymlinkOutOfTheShare(t *testing.T) {
	root, outside := share(t)
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := pull(root, []string{"/evil/secret"})
	if err != nil {
		return
	}
	tr := tar.NewReader(bytes.NewReader(out))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		t.Errorf("a pull followed a symlink out of the share and returned %s", h.Name)
	}
}

// The same three against an ordinary tree, so the refusals above are not
// bought by refusing everything.
func TestExtractPullAndRemoveInsideTheShare(t *testing.T) {
	root := t.TempDir()
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	var batch bytes.Buffer
	tw := tar.NewWriter(&batch)
	_ = tw.WriteHeader(&tar.Header{Name: "pkg", Typeflag: tar.TypeDir, Mode: 0o755})
	_ = tw.WriteHeader(&tar.Header{Name: "pkg/lib.go", Typeflag: tar.TypeReg, Mode: 0o644, Size: 2, ModTime: at})
	_, _ = tw.Write([]byte("go"))
	_ = tw.Close()

	var landed []string
	if err := extract(root, &batch, func(name string, info os.FileInfo) {
		landed = append(landed, name)
		if !info.ModTime().Equal(at) {
			t.Errorf("%s landed at %v, want %v", name, info.ModTime(), at)
		}
	}); err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(landed) != 1 || landed[0] != "/pkg/lib.go" {
		t.Errorf("landed = %v, want [/pkg/lib.go]", landed)
	}

	out, err := pull(root, []string{"/pkg/lib.go"})
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	h, err := tar.NewReader(bytes.NewReader(out)).Next()
	if err != nil || h.Name != "pkg/lib.go" || h.Size != 2 {
		t.Errorf("pull returned %+v, %v", h, err)
	}

	if err := remove(root, []string{"/pkg/lib.go"}, func(string) {}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "pkg", "lib.go")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the file is still there after remove: %v", err)
	}
}
