//go:build windows

package nfsserve

import (
	"os"
	"syscall"
)

// openShared opens a path so that holding it does not stop anything else
// deleting or renaming it.
//
// Go's os.OpenFile asks Windows for FILE_SHARE_READ|FILE_SHARE_WRITE and not
// FILE_SHARE_DELETE, so a file this process holds open can be neither removed
// nor renamed by anyone: measured 2026-09-07, both fail with "The process
// cannot access the file because it is being used by another process". That
// costs nothing for a descriptor held for one request, and would be a real
// change for one held across requests: a user's own editor or `rm` would start
// failing on a file their container had just written.
//
// With FILE_SHARE_DELETE the delete succeeds and reads through this handle keep
// working, which is the POSIX behaviour the rest of this package assumes.
func openShared(path string) (*os.File, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}

	h, err := syscall.CreateFile(name,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}
