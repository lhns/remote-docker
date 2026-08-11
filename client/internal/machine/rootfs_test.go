package machine

// Which image a machine is built from, and where the filesystem is kept.
//
// The pull itself is go-containerregistry's, and the flattening is
// mutate.Extract, which is what `docker export` does. What is ours and pinned
// here is which image gets asked for and how the result is cached.

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultImage(t *testing.T) {
	// A tagged build takes its own version, so a machine matches the binary
	// that built it -- and, because the image is part of the Spec, a client on
	// a new version builds a new machine rather than adopting an old one.
	if got := DefaultImage("v0.2.0"); got != DefaultImageRepo+":0.2.0" {
		t.Errorf("DefaultImage(v0.2.0) = %q", got)
	}

	// An untagged build takes latest rather than being refused a machine for
	// having no tag.
	for _, version := range []string{"dev", "", "1a2b3c4", "v1"} {
		if got := DefaultImage(version); got != DefaultImageRepo+":latest" {
			t.Errorf("DefaultImage(%q) = %q, want latest", version, got)
		}
	}
}

func TestReleaseTag(t *testing.T) {
	// The image is tagged without the leading v, which is what the release
	// workflow pushes. Asking for "v0.2.0" would be a tag that does not exist,
	// and the failure would arrive as a registry 404 naming nothing useful.
	tag, ok := releaseTag("v0.2.0")
	if !ok || tag != "0.2.0" {
		t.Errorf("releaseTag(v0.2.0) = %q, %v", tag, ok)
	}
	for _, version := range []string{"dev", "", "1a2b3c4", "v1"} {
		if _, ok := releaseTag(version); ok {
			t.Errorf("releaseTag(%q) claimed to be a release", version)
		}
	}
}

func TestRootfsCacheDirIsBesideTheMachines(t *testing.T) {
	t.Setenv("LOCALAPPDATA", filepath.Join("C:", "Users", "x", "AppData", "Local"))

	dir, err := rootfsCacheDir()
	if err != nil {
		t.Fatalf("rootfsCacheDir: %v", err)
	}
	// Beside the machines rather than inside one: the file outlives any machine
	// built from it, and removing a machine must not take it.
	if !strings.HasSuffix(dir, filepath.Join("remote-docker", "rootfs")) {
		t.Errorf("rootfsCacheDir = %q", dir)
	}
	if strings.Contains(dir, "machines") {
		t.Errorf("the cache lives under a machine: %q", dir)
	}
}
