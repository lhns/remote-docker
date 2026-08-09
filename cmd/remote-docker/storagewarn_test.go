package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/lhns/remote-docker/internal/client/proxy"
)

// The warning must fire for vfs and stay silent for everything else.
//
// Both halves matter. A workspace that is configured correctly must print
// nothing at all -- output belongs to the command being run -- and a workspace
// on vfs must say so where somebody is standing when they notice the slowness,
// which is any `remote-docker docker ...`, not `status` run on purpose.
func TestSlowStorageWarning(t *testing.T) {
	var buf bytes.Buffer
	warnSlowStorage(&buf, proxy.Status{Storage: "vfs"})

	got := buf.String()
	if got == "" {
		t.Fatal("vfs produced no warning")
	}
	// It has to name the driver and say what to do about it; a warning that
	// only says "slow" leaves the reader exactly where they were.
	for _, want := range []string{"vfs", "storage-driver", "WORKSPACE_DOCKERD_ARGS"} {
		if !strings.Contains(got, want) {
			t.Errorf("the warning does not mention %q:\n%s", want, got)
		}
	}

	for _, driver := range []string{"overlay2", "fuse-overlayfs", "btrfs", ""} {
		buf.Reset()
		warnSlowStorage(&buf, proxy.Status{Storage: driver})
		if buf.Len() != 0 {
			t.Errorf("storage=%q warned anyway: %s", driver, buf.String())
		}
	}
}
