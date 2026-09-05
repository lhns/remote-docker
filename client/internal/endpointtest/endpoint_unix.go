//go:build !windows

// Package endpointtest names an endpoint a test may bind, in the platform's
// own spelling. A socket under the test's own directory here; a uniquely named
// pipe on Windows, where there is no filesystem path to put one at and where
// the bind genuinely excludes.
//
// It imports testing, so it must only ever be imported from _test.go files:
// imported from the binary, it links testing's flags into it.
package endpointtest

import (
	"path/filepath"
	"testing"
)

// Endpoint is a Docker endpoint this test may bind.
func Endpoint(t testing.TB) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "docker.sock")
}
