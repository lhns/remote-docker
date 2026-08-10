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

// processAlive reports whether pid is still RUNNING.
//
// Opening the process is not the question, and answering it that way is a bug
// this file had: a process that has exited can still be opened while any
// handle to it remains, so `stop` would have waited its whole timeout and then
// reported a running session that had been gone for seconds.
//
// GetExitCodeProcess is the question. STILL_ACTIVE (259) means running;
// anything else is an exit status. The ambiguity Windows is famous for here --
// a process that genuinely exits WITH 259 reads as alive, costs nothing in
// this use: the session exits 0 or is killed, and the caller has a timeout.
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
