//go:build !windows

package dircache

import "syscall"

// The errors a write fails with on this platform, for the tests that need one
// that classifyWrite recognises.
var (
	errNoSpace   error = syscall.ENOSPC
	errBadName   error = syscall.EINVAL
	errNoSymlink error = syscall.EINVAL
)
