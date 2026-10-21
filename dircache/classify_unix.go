//go:build !windows

package dircache

import "syscall"

// diskFull is a full disk or a quota that is spent.
func diskFull(e syscall.Errno) bool { return e == syscall.ENOSPC || e == syscall.EDQUOT }

// unholdableName is a name this filesystem will not take: invalid, too long, or
// in an encoding it refuses.
func unholdableName(e syscall.Errno) bool {
	return e == syscall.EINVAL || e == syscall.ENAMETOOLONG || e == syscall.EILSEQ
}
