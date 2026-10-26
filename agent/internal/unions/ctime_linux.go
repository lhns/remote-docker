//go:build linux

package unions

import (
	"io/fs"
	"syscall"
)

// ctimeOf is an entry's change time in Unix nanoseconds, or 0 when the
// platform does not say. A container can set a file's mtime and never its
// ctime, which is why write-back decides who wrote last by it (ADR 0044).
func ctimeOf(info fs.FileInfo) int64 {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return st.Ctim.Nano()
}
