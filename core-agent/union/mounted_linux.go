//go:build linux

package union

import "golang.org/x/sys/unix"

// mountedAt reports whether anything is mounted at path: whether it is on a
// different device from its parent. That answers from outside the owning
// namespace too, through /proc/<pid>/root (test/union-probe.sh section 12).
//
// Never "the path exists": a union's directories outlive it, so a stat calls a
// union that never mounted serving, and the container binds an empty directory
// (CLAUDE.md, "A union that never mounted looks exactly like one that did").
func mountedAt(path string) bool {
	var here, up unix.Stat_t
	if err := unix.Lstat(path, &here); err != nil {
		return false
	}
	if err := unix.Lstat(path+"/..", &up); err != nil {
		return false
	}
	return here.Dev != up.Dev
}
