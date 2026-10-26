//go:build darwin

package session

import (
	"os"
	"syscall"
	"time"
)

// changeTime is a share path's ctime (dircache.Cache.ChangeTime). Stat, never
// an open: a fifo would block one.
func changeTime(r *os.Root, rel string) (time.Time, bool) {
	info, err := r.Stat(rel)
	if err != nil {
		return time.Time{}, false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(st.Ctimespec.Unix()), true
}
