//go:build unix

package nfsserve

import (
	"os"
	"syscall"
)

// inodeOf is the file's identity: its device and its inode.
//
// BOTH, because an inode is unique within one filesystem only. A share that
// crosses a mount point holds files from two, and two of them could otherwise
// report one identity -- the same failure pointed the other way.
func inodeOf(fi os.FileInfo, _ string) (uint64, uint64, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return uint64(st.Dev), uint64(st.Ino), true
}
