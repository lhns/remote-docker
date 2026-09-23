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

// processAlive reports whether pid still exists, by signal 0. os.FindProcess
// never fails on Unix, so it cannot answer this.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}
