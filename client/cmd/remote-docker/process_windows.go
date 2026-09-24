package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// killPID ends a process this one deliberately stopped being the parent of.
func killPID(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	defer func() { _ = p.Release() }()
	return p.Kill()
}

// processAlive reports whether pid is still RUNNING. An exited process can
// still be opened while any handle remains, so the exit code is what decides.
// A process that exits with 259 reads as alive; the caller has a timeout.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer func() { _ = windows.CloseHandle(h) }()

	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}

// stillActive is STILL_ACTIVE from the Windows headers, which x/sys/windows
// does not export.
const stillActive = 259
