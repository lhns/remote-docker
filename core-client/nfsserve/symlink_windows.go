//go:build windows

package nfsserve

import (
	"errors"
	"syscall"
)

// symlinkPrivileged reports whether the host refused for want of
// SeCreateSymbolicLinkPrivilege, which Developer Mode grants and an ordinary
// account does not hold.
//
// Matched by errno: os.ErrPermission does NOT match this one, so the obvious
// test silently never fires.
func symlinkPrivileged(err error) bool {
	return errors.Is(err, syscall.Errno(1314)) // ERROR_PRIVILEGE_NOT_HELD
}
