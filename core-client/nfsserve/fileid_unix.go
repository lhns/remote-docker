//go:build unix

package nfsserve

import (
	"os"
	"syscall"
)

// inodeOf is the file's identity: its device and its inode.
//
// BOTH, because an inode number is unique within one filesystem and no
// further. A share that crosses a mount point on this machine holds files from
// two of them, and two files with the same inode on different devices would
// otherwise report one identity -- which is the same failure this whole thing
// is about, pointed the other way: the client would treat two files as one.
func inodeOf(fi os.FileInfo, _ string) (uint64, uint64, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return uint64(st.Dev), uint64(st.Ino), true
}
