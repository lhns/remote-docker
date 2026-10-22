//go:build !windows

package dircache

import (
	"os"
	"syscall"
	"testing"
)

// What a failed write says about trying again, by errno. A full disk is waited
// out, a name this filesystem refuses is given up on, and anything else is
// left for the next round.
func TestClassifyWriteByErrno(t *testing.T) {
	for _, tc := range []struct {
		errno syscall.Errno
		want  fault
	}{
		{syscall.ENOSPC, faultSpace},
		{syscall.EDQUOT, faultSpace},
		{syscall.EINVAL, faultName},
		{syscall.ENAMETOOLONG, faultName},
		{syscall.EILSEQ, faultName},
		{syscall.EACCES, faultOther},
		{syscall.EBUSY, faultOther},
		{syscall.EIO, faultOther},
	} {
		err := &os.PathError{Op: "openat", Path: "x", Err: tc.errno}
		if got := classifyWrite(err); got != tc.want {
			t.Errorf("%v classified %v, want %v", tc.errno, got, tc.want)
		}
	}
}
