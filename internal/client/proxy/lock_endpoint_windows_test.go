//go:build windows

package proxy

import (
	"strings"
	"testing"
)

// A pipe name unique to this test run: named pipes are a machine-wide
// namespace, so a fixed name would collide with a real session or a parallel
// run.
func testEndpoint(t *testing.T) string {
	t.Helper()
	name := strings.ReplaceAll(t.Name(), "/", "_")
	return `\\.\pipe\remote-docker-test-` + name
}
