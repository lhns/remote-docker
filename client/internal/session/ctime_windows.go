//go:build windows

package session

import (
	"os"
	"syscall"
	"time"
	"unsafe"
)

// GetFileInformationByHandleEx is not in the standard library's syscall, and
// golang.org/x/sys is not worth a dependency for one call.
var procGetFileInformationByHandleEx = syscall.NewLazyDLL("kernel32.dll").NewProc("GetFileInformationByHandleEx")

// fileBasicInfo is FILE_BASIC_INFO: four FILETIMEs as 100ns intervals since
// 1601, then the attributes, padded to 8 bytes.
type fileBasicInfo struct {
	CreationTime, LastAccessTime, LastWriteTime, ChangeTime int64
	FileAttributes                                          uint32
	_                                                       uint32
}

// fileBasicInfoClass is FileBasicInfo in FILE_INFO_BY_HANDLE_CLASS.
const fileBasicInfoClass = 0

// changeTime is a share path's ChangeTime, which NTFS moves on every write,
// rename and attribute change and which os.Stat does not report
// (dircache.Cache.ChangeTime). Opened through the share's root, so a link
// cannot lead it out; a file another program holds without sharing cannot be
// opened, and is then not known, which keeps it.
func changeTime(r *os.Root, rel string) (time.Time, bool) {
	f, err := r.Open(rel)
	if err != nil {
		return time.Time{}, false
	}
	defer func() { _ = f.Close() }()

	var info fileBasicInfo
	ok, _, _ := procGetFileInformationByHandleEx.Call(
		f.Fd(), fileBasicInfoClass, uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
	if ok == 0 || info.ChangeTime == 0 {
		return time.Time{}, false
	}
	// 116444736000000000 is 1970-01-01 in 100ns intervals since 1601.
	return time.Unix(0, (info.ChangeTime-116444736000000000)*100), true
}
