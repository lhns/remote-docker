package unions

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lhns/remote-docker/core-agent/union"
	"github.com/lhns/remote-docker/core/workspace"
)

// The daemon's image is its account's to arrange, so the capability is read
// through a Root confined to it: a link inside the image is followed, one that
// leaves it is not. A temporary directory stands in for /proc/<pid>/root and a
// second one for the agent's own filesystem.

func write(t *testing.T, dir, rel string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, nil, 0o755); err != nil {
		t.Fatal(err)
	}
}

func link(t *testing.T, dir, rel, target string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
}

func TestCapabilityReadsAnOrdinaryImage(t *testing.T) {
	root := t.TempDir()
	if got := Capability(root); got != workspace.UnionNoDevice {
		t.Errorf("an empty image = %q, want %q", got, workspace.UnionNoDevice)
	}
	write(t, root, "dev/fuse")
	if got := Capability(root); got != workspace.UnionNoBinary {
		t.Errorf("no binary = %q, want %q", got, workspace.UnionNoBinary)
	}
	write(t, root, "usr/bin/"+union.Binary)
	if got := Capability(root); got != workspace.UnionReady {
		t.Errorf("with both = %q, want %q", got, workspace.UnionReady)
	}
}

func TestCapabilityFollowsALinkInsideTheImage(t *testing.T) {
	root := t.TempDir()
	write(t, root, "dev/fuse")
	write(t, root, "opt/tools/"+union.Binary)
	link(t, root, "sbin", "opt/tools")
	if got := Capability(root); got != workspace.UnionReady {
		t.Errorf("a relative link inside the image = %q, want %q", got, workspace.UnionReady)
	}
}

func TestCapabilityIgnoresALinkOutOfTheImage(t *testing.T) {
	outside := t.TempDir()
	write(t, outside, "dev/fuse")
	write(t, outside, "tools/"+union.Binary)

	// where spells the outside directory as a link target for an image at dir.
	for name, where := range map[string]func(dir string) string{
		"absolute": func(string) string { return outside },
		"relative": func(dir string) string {
			rel, err := filepath.Rel(dir, outside)
			if err != nil {
				t.Fatal(err)
			}
			return rel
		},
	} {
		t.Run(name, func(t *testing.T) {
			image := t.TempDir()
			write(t, image, "dev/fuse")
			link(t, image, "bin", filepath.Join(where(image), "tools"))
			if got := Capability(image); got != workspace.UnionNoBinary {
				t.Errorf("a binary behind a link out of the image = %q, want %q", got, workspace.UnionNoBinary)
			}

			image = t.TempDir()
			write(t, image, "usr/bin/"+union.Binary)
			link(t, image, "dev", where(image)+"/dev")
			if got := Capability(image); got != workspace.UnionNoDevice {
				t.Errorf("a device behind a link out of the image = %q, want %q", got, workspace.UnionNoDevice)
			}
		})
	}
}
