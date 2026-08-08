//go:build !windows

package proxy

import (
	"path/filepath"
	"testing"
)

func testEndpoint(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "docker.sock")
}
