//go:build unix

package nfsserve

import (
	"os"
	"syscall"
)

// inodeOf is the file's real inode number, which is what a fileid is meant to
// be: stable for the life of the file, and the same however the file was
// reached.
func inodeOf(fi os.FileInfo, _ string) (uint64, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Ino), true
}
