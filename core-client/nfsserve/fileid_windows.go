//go:build windows

package nfsserve

import (
	"os"
	"syscall"
)

// identityOf is the file's identity and link count on Windows, which exist and
// are simply not on os.FileInfo.
//
// NTFS gives every file a File Reference Number, and GetFileInformationByHandle
// returns it as nFileIndexHigh/nFileIndexLow. It is what os.SameFile compares
// and it is as stable as an inode. os.Stat does not carry it because reading it
// costs an open, which is the price paid here, and the same call answers
// NumberOfLinks so the count costs nothing more.
//
// FILE_FLAG_BACKUP_SEMANTICS so a directory opens at all, every share flag so
// this never blocks a writer, and no access rights, so it works on a file the
// user cannot read.
func identityOf(_ os.FileInfo, osPath string) (identity, bool) {
	if osPath == "" {
		return identity{}, false
	}
	p, err := syscall.UTF16PtrFromString(osPath)
	if err != nil {
		return identity{}, false
	}
	h, err := syscall.CreateFile(p, 0,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return identity{}, false
	}
	defer func() { _ = syscall.CloseHandle(h) }()

	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(h, &info); err != nil {
		return identity{}, false
	}
	// The volume serial plays the part of a device number: a file index is
	// unique within one volume and no further.
	return identity{
		dev:   uint64(info.VolumeSerialNumber),
		ino:   uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow),
		nlink: info.NumberOfLinks,
	}, true
}
