//go:build linux

package union

import "golang.org/x/sys/unix"

// mountedAt reports whether anything is mounted at path, by asking whether the
// path and its parent are on the same device.
//
// Cheaper and more direct than parsing mountinfo, and it answers from OUTSIDE
// the namespace that owns the mount as well as inside it: measured through
// /proc/<pid>/root on 2026-09-01, an unmounted directory shares its parent's
// device and a mounted one does not (test/union-probe.sh section 12).
//
// The distinction is load-bearing rather than pedantic. A union's directories
// are created before it is mounted and outlive it, so "the path exists" is true
// of a share that never mounted and of one whose server has died. Both then
// read as serving: the workspace declares the share ready, a container binds an
// ordinary empty directory, and the agent writes the cache into it. Everything
// looks like it works and nothing is a union -- which is exactly how a lower
// that could not mount at all went unnoticed while its child crash-looped every
// two seconds.
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
