//go:build !windows

package nfsserve

// symlinkPrivileged is Windows's alone: everywhere else a symlink needs no
// privilege, and what the host refuses it refuses at the syscall.
func symlinkPrivileged(error) bool { return false }
