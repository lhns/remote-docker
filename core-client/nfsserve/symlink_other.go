//go:build !windows

package nfsserve

// symlinkPrivileged is Windows's alone: nowhere else does a symlink need a
// privilege.
func symlinkPrivileged(error) bool { return false }
