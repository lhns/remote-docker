//go:build linux

package union

import (
	"os"
	"path/filepath"
	"testing"
)

// A daemon's account is root in its own filesystem, so a link planted in the
// path to a union must not lead the probe anywhere. /proc stands in for a mount
// the probe must not report, since it is one on every Linux and needs no
// privilege to point at. The temporary directory stands in for
// /proc/<pid>/root.

func procIsAMount(t *testing.T) {
	t.Helper()
	if !mountedAt("/", "/proc") {
		t.Skip("/proc is not a separate mount here")
	}
}

func TestMountedAtDoesNotFollowALinkToAMount(t *testing.T) {
	procIsAMount(t)

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "run"), 0o755); err != nil {
		t.Fatal(err)
	}
	// An absolute link to the agent's own root, and a relative one that climbs.
	for name, target := range map[string]string{
		"run/agent": "/",
		"run/climb": "../../../../../../../",
		"hostproc":  "/proc",
	} {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Skipf("cannot create a symlink here: %v", err)
		}
	}

	for _, p := range []string{"/run/agent/proc", "/run/climb/proc", "/hostproc"} {
		if mountedAt(root, p) {
			t.Errorf("%s was reported mounted through a link", p)
		}
	}
}

func TestMountedAtAnswersForAnOrdinaryPathBelowTheRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "run/rd-union/c/id/merged"), 0o755); err != nil {
		t.Fatal(err)
	}
	// On the same device as its parent, so not a mount, and not an error either.
	if mountedAt(root, "/run/rd-union/c/id/merged") {
		t.Error("a plain directory was reported mounted")
	}
	if mountedAt(root, "/run/rd-union/c/id/missing") {
		t.Error("a missing directory was reported mounted")
	}
}

// A link where the unions live means the listing cannot be trusted, which a
// caller deciding what may be deleted reads as every cache mounted.
func TestMountedSharesRefusesALinkWhereTheUnionsLive(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "run"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(outside, "id/merged"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "run/rd-union")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	if got, err := MountedShares(root, "0123456789abcdef"); err == nil {
		t.Errorf("listed %v through a link", got)
	}
}

// And a link below it, at a client's directory.
func TestMountedSharesRefusesALinkAtAClientsDirectory(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "run/rd-union"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "run/rd-union/0123456789abcdef")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	if got, err := MountedShares(root, "0123456789abcdef"); err == nil {
		t.Errorf("listed %v through a link", got)
	}
}
