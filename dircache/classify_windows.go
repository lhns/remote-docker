//go:build windows

package dircache

import "syscall"

// The Win32 codes, spelled out because this module imports nothing outside the
// standard library (ADR 0021).
const (
	errorDiskFull         syscall.Errno = 112  // ERROR_DISK_FULL
	errorHandleDiskFull   syscall.Errno = 39   // ERROR_HANDLE_DISK_FULL
	errorInvalidName      syscall.Errno = 123  // ERROR_INVALID_NAME
	errorFilenameExceeds  syscall.Errno = 206  // ERROR_FILENAME_EXCED_RANGE
	errorPrivilegeNotHeld syscall.Errno = 1314 // ERROR_PRIVILEGE_NOT_HELD: a symlink needs Developer Mode or elevation
)

// diskFull is a full disk.
func diskFull(e syscall.Errno) bool { return e == errorDiskFull || e == errorHandleDiskFull }

// unholdableName is a name or a link this account cannot make here.
func unholdableName(e syscall.Errno) bool {
	return e == errorInvalidName || e == errorFilenameExceeds || e == errorPrivilegeNotHeld
}
