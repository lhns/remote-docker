//go:build !linux && !darwin && !windows

package session

import (
	"os"
	"time"
)

// changeTime is not known on a platform nothing here reads a ctime for, and a
// time that is not known never writes or deletes (dircache.Cache.ChangeTime).
func changeTime(*os.Root, string) (time.Time, bool) { return time.Time{}, false }
