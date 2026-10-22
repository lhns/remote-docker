package dircache

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
)

// A body that fails half way leaves the user's file as it was and no temporary
// beside it. Writing into the file would have left it truncated.
func TestWriteUnderFailedBodyLeavesTheTargetAlone(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "main.go")
	if err := os.WriteFile(target, []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	body := io.MultiReader(strings.NewReader("half of the n"), iotest.ErrReader(errors.New("link dropped")))
	err := writeUnder(openRoot(t, root), File{Path: "/main.go", Mode: 0o644, Body: body}, nil)
	if err == nil {
		t.Fatal("a body that failed was reported as written")
	}

	got, _ := os.ReadFile(target)
	if string(got) != "before\n" {
		t.Errorf("the file holds %q, want its old content", got)
	}
	assertOnly(t, root, "main.go")
}

// A rename that is refused (here: the target is a non-empty directory) leaves
// nothing behind either.
func TestWriteUnderRefusedRenameLeavesNoTemporary(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "main.go", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := writeUnder(openRoot(t, root), File{Path: "/main.go", Mode: 0o644, Body: strings.NewReader("x")}, nil)
	if err == nil {
		t.Fatal("replacing a directory with a file was reported as written")
	}
	assertOnly(t, root, "main.go")
}

func assertOnly(t *testing.T, dir string, want string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != want {
		t.Errorf("%s holds %v, want only %s", dir, entries, want)
	}
}

// The walk that fills the cache never offers a landing in progress.
func TestWalkSkipsAWriteBackTemporary(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.go", writebackPrefix + "1f"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	walk(root, nil, func(e Entry) { got = append(got, e.Path) })
	if len(got) != 1 || got[0] != "a.go" {
		t.Errorf("walk offered %v, want only a.go", got)
	}
}
