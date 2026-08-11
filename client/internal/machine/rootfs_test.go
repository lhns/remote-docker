package machine

// Which file a machine is built from, and where it is kept.
//
// Downloading is the one thing this project does that looks like installing, so
// the rules about it are pinned: one immutable file per release, named by
// architecture, cached under the version that asked for it.

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestRootfsURL(t *testing.T) {
	// A tagged build takes its own tag, so the machine matches the client.
	got, err := RootfsURL("v0.2.0", "amd64")
	if err != nil {
		t.Fatalf("RootfsURL: %v", err)
	}
	if !strings.Contains(got, "/download/v0.2.0/") || !strings.HasSuffix(got, "workspace-rootfs-amd64.tar.gz") {
		t.Errorf("RootfsURL(v0.2.0) = %q", got)
	}

	// A development build takes the latest release rather than being refused a
	// machine for having no tag.
	for _, version := range []string{"dev", "", "1a2b3c4"} {
		got, err := RootfsURL(version, "arm64")
		if err != nil {
			t.Fatalf("RootfsURL(%q): %v", version, err)
		}
		if !strings.Contains(got, "/latest/download/") {
			t.Errorf("RootfsURL(%q) = %q, want the latest release", version, got)
		}
		if !strings.HasSuffix(got, "workspace-rootfs-arm64.tar.gz") {
			t.Errorf("RootfsURL(%q) asked for the wrong architecture: %q", version, got)
		}
	}

	// An architecture with nothing published is refused rather than sent an
	// amd64 filesystem, which would import and then execute nothing.
	if _, err := RootfsURL("v0.2.0", "riscv64"); err == nil {
		t.Error("an architecture with no published filesystem was accepted")
	} else if !strings.Contains(err.Error(), "--rootfs") {
		t.Errorf("the error does not say what to do instead: %v", err)
	}
}

func TestRootfsCacheName(t *testing.T) {
	name := "workspace-rootfs-amd64.tar.gz"

	// Keyed by version, so a machine on one version does not silently get
	// another's filesystem.
	if got := rootfsCacheName("v0.2.0", name); got != "v0.2.0-"+name {
		t.Errorf("rootfsCacheName = %q", got)
	}
	// An untagged build is stored as what it actually fetched. It is the one
	// entry that can go stale, and deleting it is how a newer one is asked for.
	if got := rootfsCacheName("dev", name); got != "latest-"+name {
		t.Errorf("rootfsCacheName(dev) = %q, want latest-", got)
	}
	if rootfsCacheName("v0.2.0", name) == rootfsCacheName("v0.3.0", name) {
		t.Error("two versions share one cache entry")
	}
}

func TestRootfsCacheDirIsBesideTheMachines(t *testing.T) {
	t.Setenv("LOCALAPPDATA", filepath.Join("C:", "Users", "x", "AppData", "Local"))

	dir, err := rootfsCacheDir()
	if err != nil {
		t.Fatalf("rootfsCacheDir: %v", err)
	}
	// Beside the machines rather than inside one: the file outlives any machine
	// built from it, and `rm` of a machine must not take it.
	if !strings.HasSuffix(dir, filepath.Join("remote-docker", "rootfs")) {
		t.Errorf("rootfsCacheDir = %q", dir)
	}
	if strings.Contains(dir, "machines") {
		t.Errorf("the download cache lives under a machine: %q", dir)
	}
}

func TestIsRelease(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"v0.1.0", true},
		{"v1.2.3-rc1", true},
		{"dev", false},
		{"", false},
		// A commit sha is not a release, however much it looks like a version.
		{"1a2b3c4", false},
		// A tag with no dot is not one either -- goreleaser does not make them.
		{"v1", false},
	} {
		if got := isRelease(tc.in); got != tc.want {
			t.Errorf("isRelease(%q) = %v", tc.in, got)
		}
	}
}
