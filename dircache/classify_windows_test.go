//go:build windows

package dircache

import (
	"os"
	"syscall"
	"testing"
)

// What a failed write says about trying again, by Win32 code (ADR 0044): a full
// disk is waited out, a name or a link this account cannot make is given up on,
// and a file another program holds is left for the next round.
func TestClassifyWriteByWin32Code(t *testing.T) {
	for _, tc := range []struct {
		code syscall.Errno
		name string
		want fault
	}{
		{112, "ERROR_DISK_FULL", faultSpace},
		{39, "ERROR_HANDLE_DISK_FULL", faultSpace},
		{123, "ERROR_INVALID_NAME", faultName},
		{206, "ERROR_FILENAME_EXCED_RANGE", faultName},
		{1314, "ERROR_PRIVILEGE_NOT_HELD", faultName},
		{5, "ERROR_ACCESS_DENIED", faultOther},
		{32, "ERROR_SHARING_VIOLATION", faultOther},
		{33, "ERROR_LOCK_VIOLATION", faultOther},
	} {
		err := &os.PathError{Op: "CreateFile", Path: "x", Err: tc.code}
		if got := classifyWrite(err); got != tc.want {
			t.Errorf("%s (%d) classified %v, want %v", tc.name, uint32(tc.code), got, tc.want)
		}
	}
}
