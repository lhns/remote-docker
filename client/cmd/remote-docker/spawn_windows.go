//go:build windows

package main

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// detach makes the child outlive this process and ignore Ctrl-C in this
// terminal: it gets a console of its own, with no window.
//
// A console and not DETACHED_PROCESS, because Restart Manager stops a console
// program by sending CTRL_C_EVENT to its console, and a process with none
// cannot be asked: an MSI upgrade then reports it cannot close the session
// (ADR 0048). No CREATE_NEW_PROCESS_GROUP either, which disables Ctrl-C in the
// child; the console being its own already keeps the terminal's Ctrl-C out.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
}

var procSetConsoleCtrlHandler = windows.NewLazySystemDLL("kernel32.dll").NewProc("SetConsoleCtrlHandler")

// acceptCtrlC undoes an "ignore Ctrl-C" inherited from whoever started this
// process, which would hide the CTRL_C_EVENT Restart Manager sends a session.
func acceptCtrlC() {
	_, _, _ = procSetConsoleCtrlHandler.Call(0, 0)
}
