//go:build windows && !rdstandin

package machine

import (
	"context"
	"io"
	"path/filepath"
)

// What the service drives Hyper-V with, and where its disk comes from. The
// build tag rdstandin swaps both for a stand-in (broker_standin_windows.go),
// which only the installer's CI build carries.

func serviceExecutor(root string) hyperVExecutor { return serviceHyperV(root) }

func serviceDisk(ctx context.Context, root, proxy string) (string, error) {
	return ensureFlatcar(ctx, flatcarPinned, filepath.Join(root, "rootfs"), proxy, io.Discard)
}
