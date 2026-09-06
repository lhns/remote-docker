//go:build unix

package nfsserve

import (
	"os"
	"syscall"
)

// identityOf is the file's identity and its link count, from the stat the
// caller already has.
//
// BOTH device and inode, because an inode is unique within one filesystem
// only. A share that crosses a mount point holds files from two, and two of
// them could otherwise report one identity -- the same failure pointed the
// other way.
func identityOf(fi os.FileInfo, _ string) (identity, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return identity{}, false
	}
	// Converted rather than assigned: Nlink is uint64 on Linux and uint16 on
	// darwin, and Dev and Ino differ in width across the same set.
	return identity{dev: uint64(st.Dev), ino: uint64(st.Ino), nlink: uint32(st.Nlink)}, true
}
