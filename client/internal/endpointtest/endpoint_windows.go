//go:build windows

// Package endpointtest names an endpoint a test may bind, in the platform's
// own spelling. A socket under the test's own directory on Unix; a uniquely
// named pipe here, where there is no filesystem path to put one at and where
// the bind genuinely excludes.
//
// It imports testing, so it must only ever be imported from _test.go files:
// imported from the binary, it links testing's flags into it.
package endpointtest

import (
	"strings"
	"testing"
)

// Endpoint is a Docker endpoint this test may bind. Named pipes are a
// machine-wide namespace, so a fixed name would collide with a real session or
// a parallel run.
func Endpoint(t testing.TB) string {
	t.Helper()
	name := strings.ReplaceAll(t.Name(), "/", "_")
	return `\\.\pipe\remote-docker-test-` + name
}
