//go:build !windows

package main

import (
	"os"
	"syscall"
)

// killPID ends a process this one deliberately stopped being the parent of.
func killPID(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}

// processAlive reports whether pid still exists.
//
// Signal 0 checks for the process without touching it. os.FindProcess cannot
// answer this on Unix, where it never fails because a pid is not a handle,
// which is why this is build-tagged rather than shared.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}
