package cache

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
	if _, err := WriteTar(TarFilesFrom(root, []string{"/pkg/lib.go"}), &buf); err != nil {
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
	if _, err := WriteTar(files, &buf); err != nil {
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
	if _, err := WriteTar(TarFilesFrom(root, []string{"/adir"}), &buf); err != nil {
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

// Under Carry a file that cannot be read is named and left out, and the rest of
// the archive is whole; one that has gone is left out without a word.
func TestWriteTarNamingNamesAnUnreadableFileAndKeepsTheRest(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"good", "bad"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	broken := errors.New("input/output error")
	open := func(p string) (*os.File, error) {
		if filepath.Base(p) == "bad" {
			return nil, broken
		}
		return os.Open(p)
	}

	var buf bytes.Buffer
	unreadable, err := WriteTarNaming(open, TarFilesFrom(root, []string{"/bad", "/gone", "/good"}), &buf)
	if err != nil {
		t.Fatalf("WriteTarNaming: %v", err)
	}
	if len(unreadable) != 1 || unreadable[0].Name != "bad" || !errors.Is(unreadable[0].Err, broken) {
		t.Errorf("unreadable = %+v, want only bad, with its error", unreadable)
	}
	tr := tar.NewReader(&buf)
	header, err := tr.Next()
	if err != nil || header.Name != "good" {
		t.Fatalf("first entry = %v, %v; want good", header, err)
	}
	body, _ := io.ReadAll(tr)
	if string(body) != "good" {
		t.Errorf("good holds %q", body)
	}
	if _, err := tr.Next(); err != io.EOF {
		t.Errorf("the archive holds more than good: %v", err)
	}
}

// A tar carries a time to the second, so what lands is the time here rounded;
// the answer is the time as it was read, which is the version the entry holds.
// Write-back tells the copy it sent by both (ADR 0044).
func TestWriteTarCarriesTheSecondAndReportsWhatItRead(t *testing.T) {
	root := t.TempDir()
	full := filepath.Join(root, "a.go")
	if err := os.WriteFile(full, []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 9, 2, 10, 30, 0, 700_000_000, time.UTC)
	if err := os.Chtimes(full, when, when); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(full)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	read, err := WriteTar(TarFilesFrom(root, []string{"a.go", "missing.go"}), &buf)
	if err != nil {
		t.Fatalf("WriteTar: %v", err)
	}
	if len(read) != 1 || read[0].Name != "a.go" || read[0].Size != info.Size() || !read[0].ModTime.Equal(info.ModTime()) {
		t.Errorf("read = %+v, want a.go as it was stat'ed (%d bytes, %v)", read, info.Size(), info.ModTime())
	}
	header, err := tar.NewReader(&buf).Next()
	if err != nil {
		t.Fatal(err)
	}
	if want := info.ModTime().Round(time.Second); !header.ModTime.Equal(want) {
		t.Errorf("the entry's time is %v, want %v, the time here to the second", header.ModTime, want)
	}
}
