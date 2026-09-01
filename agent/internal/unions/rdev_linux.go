//go:build linux

package unions

import (
	"io/fs"
	"syscall"
)

// rdev is the device number of a special file, which for an overlay's whiteout
// is zero: a character device 0:0.
//
// Split by platform because the field is in a Unix-only stat structure, and the
// agent's module still has to build on the development machine, which has no
// Docker and no overlay either.
func rdev(info fs.FileInfo) uint64 {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 1 // not something this platform can answer for, so not a whiteout
	}
	return uint64(st.Rdev)
}
