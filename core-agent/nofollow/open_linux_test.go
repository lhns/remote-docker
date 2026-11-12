//go:build linux

package nofollow

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"golang.org/x/sys/unix"
)

func mkdirs(t *testing.T, dir, rel string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, rel), 0o755); err != nil {
		t.Fatal(err)
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
}

func opens(trusted, rel string) bool {
	fd, err := Open(trusted, rel)
	if err == nil {
		_ = unix.Close(fd)
	}
	return err == nil
}

func TestOpenReachesAnOrdinaryPath(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "a/b")
	if err := os.WriteFile(filepath.Join(root, "a/b/f"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"a/b/f", "/a/b", "a", "", "/", "a/./b/../b/f"} {
		if !opens(root, rel) {
			t.Errorf("Open(%q) failed on an ordinary path", rel)
		}
	}
	if opens(root, "a/missing") {
		t.Error("a missing path opened")
	}
}

// The trusted directory is the agent's own choice and may be a link, as
// /proc/<pid>/root is.
func TestOpenFollowsTheTrustedDirectory(t *testing.T) {
	tree := t.TempDir()
	mkdirs(t, tree, "a")
	link := filepath.Join(t.TempDir(), "root")
	symlink(t, tree, link)
	if !opens(link, "a") {
		t.Error("a link as the trusted directory was refused")
	}
}

// A link at any depth is refused, whether it points outside or inside, absolute
// or relative.
func TestOpenRefusesALinkInAnyComponent(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	mkdirs(t, root, "a/b")
	mkdirs(t, root, "tree")
	mkdirs(t, outside, "x")

	for name, c := range map[string]struct{ link, target, rel string }{
		"absolute, intermediate": {"a/out", outside, "a/out/x"},
		"absolute, last":         {"a/out", outside, "a/out"},
		"relative, outside":      {"a/up", "../../" + filepath.Base(outside), "a/up/x"},
		"relative, inside":       {"a/in", "../tree", "a/in"},
		"first component":        {"top", outside, "top/x"},
	} {
		t.Run(name, func(t *testing.T) {
			link := filepath.Join(root, c.link)
			symlink(t, c.target, link)
			defer func() { _ = os.Remove(link) }()
			if opens(root, c.rel) {
				t.Errorf("Open(%q) went through the link at %s", c.rel, c.link)
			}
		})
	}
}

func TestOpenReportsALinkAsOne(t *testing.T) {
	root := t.TempDir()
	symlink(t, "/", filepath.Join(root, "l"))
	fd, err := Open(root, "l")
	if err == nil {
		_ = unix.Close(fd)
		t.Fatal("a link opened")
	}
	if !errors.Is(err, ErrLink) {
		t.Errorf("error = %v, want ErrLink", err)
	}
}

// ".." is absorbed before anything is opened.
func TestOpenDoesNotClimbOutOfTheTrustedDirectory(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	mkdirs(t, base, "root")
	mkdirs(t, base, "sibling")
	if opens(root, "/../sibling") {
		t.Error("Open climbed out of the trusted directory")
	}
}

// Dev tells a mount from its parent, and does not follow a link to find one.
func TestDevDoesNotFollowALinkToAMount(t *testing.T) {
	self, parent, err := Dev("/", "/proc")
	if err != nil || self == parent {
		t.Skipf("/proc is not a separate mount here (%d, %d, %v)", self, parent, err)
	}

	root := t.TempDir()
	symlink(t, "/", filepath.Join(root, "host"))
	if _, _, err := Dev(root, "/host/proc"); err == nil {
		t.Error("Dev followed a link to /proc")
	}

	// And the same path without the link is the mount it is.
	if self, parent, err := Dev("/", "/proc"); err != nil || self == parent {
		t.Errorf("Dev(/proc) = %d, %d, %v", self, parent, err)
	}
}

func TestReadDirListsWithoutFollowing(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	mkdirs(t, root, "d/one")
	mkdirs(t, root, "d/two")
	mkdirs(t, outside, "secret")

	got, err := ReadDir(root, "d")
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	if !slices.Equal(got, []string{"one", "two"}) {
		t.Errorf("ReadDir = %v", got)
	}

	symlink(t, outside, filepath.Join(root, "link"))
	if names, err := ReadDir(root, "link"); err == nil {
		t.Errorf("ReadDir listed %v through a link", names)
	}
	if err := os.WriteFile(filepath.Join(root, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if names, err := ReadDir(root, "file"); err == nil {
		t.Errorf("ReadDir listed %v of a file", names)
	}
}
