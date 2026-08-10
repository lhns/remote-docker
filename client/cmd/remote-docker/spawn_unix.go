//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// detach makes the child outlive this process and survive Ctrl-C in the
// terminal that started it.
//
// Setsid puts it in its own session with no controlling terminal, so it is not
// in our process group and SIGINT from the terminal never reaches it. Without
// that, starting a daemon and then pressing Ctrl-C on the next command would
// take the daemon down with it.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
