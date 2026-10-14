//go:build linux

package union

import (
	"os"
	"syscall"
	"testing"

	"github.com/lhns/remote-docker/core/workspace"
)

// The merged root takes its mode and owner from the upper, so the upper is what
// openUpper settles: wide, and the share's owner. Chowned to this process's
// own ids, the one chown an unprivileged test may do.
func TestMakeLayersGivesTheUpperItsOwner(t *testing.T) {
	spec := Spec{
		Export: "/m/00112233445566ff", Client: "aabbccdd", Port: 30001,
		CacheDir: t.TempDir(), Read: workspace.ReadCached,
		UID: uint32(os.Getuid()), GID: uint32(os.Getgid()),
	}
	if err := os.MkdirAll(spec.Upper(), 0o755); err != nil {
		t.Fatal(err)
	}
	// Twice: the child runs on every serve.
	for range 2 {
		if err := openUpper(spec); err != nil {
			t.Fatal(err)
		}
	}

	info, err := os.Lstat(spec.Upper())
	if err != nil {
		t.Fatal(err)
	}
	st := info.Sys().(*syscall.Stat_t)
	if st.Uid != spec.UID || st.Gid != spec.GID || info.Mode().Perm() != 0o777 {
		t.Errorf("upper is %d:%d %v, want %d:%d 0777", st.Uid, st.Gid, info.Mode().Perm(), spec.UID, spec.GID)
	}
}
