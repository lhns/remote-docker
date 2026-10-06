package union

import (
	"os"
	"path"
	"runtime"
	"testing"
)

// No union directory is no unions; one that cannot be read is not, because a
// caller deciding what may be deleted must keep what it cannot see.
func TestMountedSharesTellsNoneFromCannotTell(t *testing.T) {
	root := t.TempDir()
	if got, err := MountedShares(root, "0123456789abcdef"); err != nil || len(got) != 0 {
		t.Errorf("with no union directory: %v, %v; want none and no error", got, err)
	}

	if runtime.GOOS != "linux" {
		t.Skip("Windows reports listing a file as not existing; unions run on Linux only")
	}
	if err := os.MkdirAll(path.Dir(path.Join(root, Root)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path.Join(root, Root), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := MountedShares(root, "0123456789abcdef"); err == nil {
		t.Errorf("an unreadable union directory answered %v with no error", got)
	}
}
