//go:build linux

package mount

import "syscall"

// IsMounted reports whether a path is a mount point, by comparing its device
// with its parent's. A path on a different device from its parent is where a
// filesystem was mounted.
func IsMounted(path string) bool {
	var self, parent syscall.Stat_t
	if err := syscall.Stat(path, &self); err != nil {
		return false
	}
	if err := syscall.Stat(path+"/..", &parent); err != nil {
		return false
	}
	return self.Dev != parent.Dev
}
