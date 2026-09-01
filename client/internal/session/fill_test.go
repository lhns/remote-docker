package session

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lhns/remote-docker/client/internal/cachefill"
	"github.com/lhns/remote-docker/core/workspace"
)

// The batch a fill sends is a tar, optionally compressed, and the frame states
// the length of what is ACTUALLY sent -- so whatever builds the bytes has to
// encode them. A payload whose length describes the tar and whose contents are
// a gzip stream desynchronises the channel for everything after it.
func TestTarOfEncodesWhatItSays(t *testing.T) {
	root := t.TempDir()
	// Compressible on purpose: a tar of random bytes is larger compressed, and
	// the assertion below is about the encoding rather than the ratio.
	body := strings.Repeat("package main // and the same line again\n", 200)
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	entries := []cachefill.Entry{{Path: "main.go", Size: int64(len(body))}}

	plain, err := tarOf(root, entries, workspace.CodecNone)
	if err != nil {
		t.Fatalf("tarOf: %v", err)
	}
	if names := tarNames(t, bytes.NewReader(plain)); len(names) != 1 || names[0] != "main.go" {
		t.Fatalf("the plain batch held %v", names)
	}

	zipped, err := tarOf(root, entries, workspace.CodecGzip)
	if err != nil {
		t.Fatalf("tarOf gzip: %v", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(zipped))
	if err != nil {
		t.Fatalf("the batch is not a gzip stream: %v", err)
	}
	if names := tarNames(t, zr); len(names) != 1 || names[0] != "main.go" {
		t.Fatalf("the compressed batch held %v", names)
	}
	if err := zr.Close(); err != nil {
		t.Errorf("closing the gzip reader: %v", err)
	}

	// The point of doing it at all, on the kind of content a source tree is.
	if len(zipped) >= len(plain) {
		t.Errorf("compressed to %d bytes from %d, which is no saving", len(zipped), len(plain))
	}
}

// A tar cut short reads as a corrupt archive rather than as a short file, so
// the compressor's footer has to be written before the buffer is measured.
func TestTarOfClosesTheCompressor(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	zipped, err := tarOf(root, []cachefill.Entry{{Path: "a.go", Size: 1}}, workspace.CodecGzip)
	if err != nil {
		t.Fatalf("tarOf: %v", err)
	}

	zr, err := gzip.NewReader(bytes.NewReader(zipped))
	if err != nil {
		t.Fatal(err)
	}
	// Reading to EOF is what checks the footer: a stream missing it fails here
	// with ErrUnexpectedEOF rather than at the header.
	if _, err := io.Copy(io.Discard, zr); err != nil {
		t.Errorf("the compressed batch does not end cleanly: %v", err)
	}
}

func tarNames(t *testing.T, r io.Reader) []string {
	t.Helper()
	var names []string
	tr := tar.NewReader(r)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return names
		}
		if err != nil {
			t.Fatalf("reading the batch: %v", err)
		}
		names = append(names, header.Name)
	}
}
