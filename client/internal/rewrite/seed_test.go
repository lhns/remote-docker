package rewrite

import (
	"archive/tar"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// entries reads a tar stream into name -> content, which is what every
// assertion below is about.
func entries(t *testing.T, r io.Reader) map[string]string {
	t.Helper()
	out := map[string]string{}
	tr := tar.NewReader(r)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("reading the tar: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("reading %s: %v", header.Name, err)
		}
		out[header.Name] = string(body)
	}
}

// A directory is streamed as its own contents, because the copy IS the
// directory: what the caller extracts under is the volume root.
func TestTarTree(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "pkg", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{
		"main.go":             "package main",
		"pkg/lib.go":          "package pkg",
		"pkg/deep/nested.txt": "down here",
		"empty.txt":           "",
	} {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(path)), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	tree := tarTree(root)
	defer tree.Close()
	got := entries(t, tree)

	for name, want := range map[string]string{
		"main.go":             "package main",
		"pkg/lib.go":          "package pkg",
		"pkg/deep/nested.txt": "down here",
		"empty.txt":           "",
	} {
		if content, ok := got[name]; !ok {
			t.Errorf("%s is not in the stream; it holds %v", name, keys(got))
		} else if content != want {
			t.Errorf("%s = %q, want %q", name, content, want)
		}
	}

	// Directories travel too, so an empty one in the project is still there
	// in the copy, and the names are slash-separated whatever this machine
	// spells paths with.
	if _, ok := got["pkg/"]; !ok {
		t.Errorf("the directories are missing; the stream holds %v", keys(got))
	}
	if _, ok := got["."]; ok {
		t.Error("the root named itself in the stream")
	}
}

// A single file is streamed under its own name, which is what the mount's
// subpath then selects (ADR 0039).
func TestTarTreeOfASingleFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, []byte("key: value"), 0o644); err != nil {
		t.Fatal(err)
	}

	tree := tarTree(path)
	defer tree.Close()

	got := entries(t, tree)
	if len(got) != 1 || got["config.yaml"] != "key: value" {
		t.Errorf("stream = %v, want just config.yaml", got)
	}
}

// A path that is not there fails the stream rather than producing an empty
// one: a container starting against an empty copy is the failure this whole
// mode has to avoid.
func TestTarTreeReportsAMissingRoot(t *testing.T) {
	tree := tarTree(filepath.Join(t.TempDir(), "absent"))
	defer tree.Close()

	if _, err := io.ReadAll(tree); err == nil {
		t.Error("a missing directory produced a stream with no error")
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
