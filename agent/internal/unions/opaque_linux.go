//go:build linux

package unions

import (
	"fmt"
	"os"
	"syscall"
)

// opaqueXattrs are the attributes an overlay sets on a directory that hides what
// its lower held: the kernel overlay's, and the two fuse-overlayfs may use
// depending on whether it runs as root. The value is "y" for each.
var opaqueXattrs = []string{
	"trusted.overlay.opaque",
	"user.overlay.opaque",
	"user.fuseoverlayfs.opaque",
}

// opaqueDir reports whether the directory dir of r carries an overlay's opaque
// mark. An attribute that cannot be read is not evidence of one.
//
// The directory is opened through r and asked by descriptor, so that a link in
// the cache layer cannot steer the read elsewhere.
func opaqueDir(r *os.Root, dir string) bool {
	f, err := r.Open(dir)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	// The descriptor's own path: package syscall has no getxattr by
	// descriptor, and this names the open directory rather than looking one up.
	self := fmt.Sprintf("/proc/self/fd/%d", f.Fd())

	var buf [4]byte
	for _, name := range opaqueXattrs {
		if n, err := syscall.Getxattr(self, name, buf[:]); err == nil && n > 0 && buf[0] == 'y' {
			return true
		}
	}
	return false
}
