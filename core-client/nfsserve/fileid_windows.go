//go:build windows

package nfsserve

import (
	"os"
	"syscall"
)

// inodeOf is the file's identity on Windows, which exists and simply is not on
// os.FileInfo.
//
// NTFS gives every file a File Reference Number -- its MFT record plus a
// sequence number -- and GetFileInformationByHandle returns it as
// nFileIndexHigh/nFileIndexLow. It is what os.SameFile compares, and it is as
// stable as an inode: unchanged by renaming, and reused only after the record
// is freed.
//
// Go's os.Stat does not carry it because reading it costs an open, which is the
// cost paid here. Correctness is worth it: without a stable number the client
// concludes a file was replaced and every open descriptor on it goes stale.
//
// FILE_FLAG_BACKUP_SEMANTICS so a directory can be opened at all, and every
// share flag so this never blocks a writer. No access rights are requested --
// the identity is readable from a handle opened for nothing, which keeps this
// from failing on a file the user may not read.
func inodeOf(_ os.FileInfo, osPath string) (uint64, bool) {
	if osPath == "" {
		return 0, false
	}
	p, err := syscall.UTF16PtrFromString(osPath)
	if err != nil {
		return 0, false
	}
	h, err := syscall.CreateFile(p, 0,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return 0, false
	}
	defer func() { _ = syscall.CloseHandle(h) }()

	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(h, &info); err != nil {
		return 0, false
	}
	return uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow), true
}
