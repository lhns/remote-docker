//go:build linux

package supervise

import "syscall"

// mountTmpfs mounts a tmpfs at path.
//
// exec because runc executes from the exec-root, and 0755 rather than the
// kernel's tmpfs default of 1777. The same two the per-account daemons pass as
// --tmpfs (agent/internal/daemons.ExecRoot).
func mountTmpfs(path string) error {
	return syscall.Mount("tmpfs", path, "tmpfs", 0, "mode=755")
}

// mountedAt reports whether anything is mounted at path, by asking whether the
// path and its parent are on the same device. core-agent/union's mountedAt is
// the same question, unexported there because that module holds the union's
// concerns rather than this one's.
func mountedAt(path string) bool {
	var here, up syscall.Stat_t
	if err := syscall.Lstat(path, &here); err != nil {
		return false
	}
	if err := syscall.Lstat(path+"/..", &up); err != nil {
		return false
	}
	return here.Dev != up.Dev
}
