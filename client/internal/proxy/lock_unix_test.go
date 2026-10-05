//go:build !windows

package proxy

import (
	"os"
	"path/filepath"
	"testing"
)

// A uid with no passwd entry gets HOME=/ in a container, and / is not
// writable. An explicit endpoint must not need $HOME at all: the Kubernetes
// client pod failed with `mkdir /.local: permission denied`.
func TestListenOnAnExplicitEndpointNeedsNoHome(t *testing.T) {
	dir := t.TempDir()
	notADir := filepath.Join(dir, "file")
	if err := os.WriteFile(notADir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", filepath.Join(notADir, "home")) // no mkdir can succeed under it
	t.Setenv("XDG_RUNTIME_DIR", "")

	endpoint := filepath.Join(dir, "state", "docker.sock")
	l, err := Listen(endpoint)
	if err != nil {
		t.Fatalf("Listen(%s) with an unusable HOME: %v", endpoint, err)
	}
	_ = l.Close()
}
