//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// detach makes the child outlive this process and ignore Ctrl-C in this
// terminal: Setsid leaves it no controlling terminal.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
