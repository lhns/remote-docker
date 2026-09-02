//go:build windows

package nfsserve

import (
	"os"
	"syscall"
)

// inodeOf is the file's identity on Windows, which exists and is simply not on
// os.FileInfo.
//
// NTFS gives every file a File Reference Number, and GetFileInformationByHandle
// returns it as nFileIndexHigh/nFileIndexLow. It is what os.SameFile compares
// and it is as stable as an inode. os.Stat does not carry it because reading it
// costs an open, which is the price paid here.
//
// FILE_FLAG_BACKUP_SEMANTICS so a directory opens at all, every share flag so
// this never blocks a writer, and no access rights, so it works on a file the
// user cannot read.
func inodeOf(_ os.FileInfo, osPath string) (uint64, uint64, bool) {
	if osPath == "" {
		return 0, 0, false
	}
	p, err := syscall.UTF16PtrFromString(osPath)
	if err != nil {
		return 0, 0, false
	}
	h, err := syscall.CreateFile(p, 0,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return 0, 0, false
	}
	defer func() { _ = syscall.CloseHandle(h) }()

	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(h, &info); err != nil {
		return 0, 0, false
	}
	// The volume serial plays the part of a device number: a file index is
	// unique within one volume and no further.
	return uint64(info.VolumeSerialNumber),
		uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow), true
}
