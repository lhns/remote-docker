//go:build windows

package main

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// detach makes the child outlive this process and survive Ctrl-C in the
// terminal that started it.
//
// DETACHED_PROCESS gives it no console at all, so it cannot inherit ours and
// cannot be killed when ours closes. CREATE_NEW_PROCESS_GROUP additionally
// keeps Ctrl-C in this terminal from reaching it -- without which starting a
// daemon and then pressing Ctrl-C on the next command would take the daemon
// down with it.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
	}
}
