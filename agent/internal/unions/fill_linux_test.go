//go:build linux

package unions

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// A mapping of a file stays readable while a landing rewrites it with the same
// content, which is what a refill is. Emptying the file first is a SIGBUS that
// kills the test binary, so a regression fails loudly rather than by assertion.
func TestLandingKeepsAMappedFileReadable(t *testing.T) {
	root := t.TempDir()
	content := "0123456789abcdef0123456789abcdef"
	land(t, root, "pack", content)

	f, err := os.Open(filepath.Join(root, "pack"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	mem, err := syscall.Mmap(int(f.Fd()), 0, len(content), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Munmap(mem) }()

	for range 50 {
		land(t, root, "pack", content)
		if string(mem) != content {
			t.Fatalf("the mapping reads %q during a refill, want %q", mem, content)
		}
	}
}
