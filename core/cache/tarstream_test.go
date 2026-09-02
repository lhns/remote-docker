package cache

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The stream is shared because both ends must produce the same thing:
// write-back compares a change's modification time against the baseline the
// fill recorded, so an mtime that did not survive would make a file the
// container never touched look changed.
func TestWriteTarPreservesNameAndTime(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pkg", "lib.go"), []byte("package pkg\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 9, 2, 10, 30, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(root, "pkg", "lib.go"), when, when); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := WriteTar(TarFilesFrom(root, []string{"/pkg/lib.go"}), &buf); err != nil {
		t.Fatalf("WriteTar: %v", err)
	}

	tr := tar.NewReader(&buf)
	header, err := tr.Next()
	if err != nil {
		t.Fatalf("reading the archive: %v", err)
	}
	// Share-relative and slash-separated, whatever the building machine uses.
	if header.Name != "pkg/lib.go" {
		t.Errorf("name = %q, want pkg/lib.go", header.Name)
	}
	if !header.ModTime.Equal(when) {
		t.Errorf("mtime = %v, want %v", header.ModTime, when)
	}
	// Ownership is the sender's and means nothing on the other side.
	if header.Uid != 0 || header.Gid != 0 || header.Uname != "" || header.Gname != "" {
		t.Errorf("ownership leaked: uid=%d gid=%d uname=%q gname=%q",
			header.Uid, header.Gid, header.Uname, header.Gname)
	}
	body, err := io.ReadAll(tr)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "package pkg\n" {
		t.Errorf("contents = %q", body)
	}
	if _, err := tr.Next(); err != io.EOF {
		t.Errorf("the archive did not end after one file: %v", err)
	}
}

// One file that cannot be opened must not cost the batch. On the client that is
// a file another process holds, which is ordinary on Windows; on the agent it is
// one the container removed between being listed and being read.
func TestWriteTarSkipsWhatItCannotRead(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "here.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	files := TarFilesFrom(root, []string{"/gone.go", "/here.go"})
	if err := WriteTar(files, &buf); err != nil {
		t.Fatalf("a missing file failed the whole archive: %v", err)
	}

	tr := tar.NewReader(&buf)
	header, err := tr.Next()
	if err != nil {
		t.Fatalf("the archive is empty: %v", err)
	}
	if header.Name != "here.go" {
		t.Errorf("name = %q, want the file that exists", header.Name)
	}
	if _, err := tr.Next(); err != io.EOF {
		t.Errorf("want one entry, got more: %v", err)
	}
}

// A directory named in the list is not a file and is skipped rather than
// written as an entry a reader would then try to open.
func TestWriteTarSkipsWhatIsNotRegular(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := WriteTar(TarFilesFrom(root, []string{"/adir"}), &buf); err != nil {
		t.Fatalf("WriteTar: %v", err)
	}
	if _, err := tar.NewReader(&buf).Next(); err != io.EOF {
		t.Errorf("a directory was written into the archive: %v", err)
	}
}

// TarFilesFrom takes the leading slash off and joins for this platform, so the
// same share-relative name works from both ends.
func TestTarFilesFrom(t *testing.T) {
	files := TarFilesFrom("/srv/project", []string{"/a.go", "b/c.go"})
	if len(files) != 2 {
		t.Fatalf("got %d files", len(files))
	}
	if files[0].Name != "a.go" || files[1].Name != "b/c.go" {
		t.Errorf("names = %q, %q", files[0].Name, files[1].Name)
	}
	if want := filepath.Join("/srv/project", "a.go"); files[0].Path != want {
		t.Errorf("path = %q, want %q", files[0].Path, want)
	}
}
