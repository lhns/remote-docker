//go:build linux

package unions

import "syscall"

// opaqueXattrs are the attributes an overlay sets on a directory that hides what
// its lower held: the kernel overlay's, and the two fuse-overlayfs may use
// depending on whether it runs as root. The value is "y" for each.
var opaqueXattrs = []string{
	"trusted.overlay.opaque",
	"user.overlay.opaque",
	"user.fuseoverlayfs.opaque",
}

// opaqueDir reports whether the directory at p carries an overlay's opaque mark.
// An attribute that cannot be read is not evidence of one.
func opaqueDir(p string) bool {
	var buf [4]byte
	for _, name := range opaqueXattrs {
		if n, err := syscall.Getxattr(p, name, buf[:]); err == nil && n > 0 && buf[0] == 'y' {
			return true
		}
	}
	return false
}
