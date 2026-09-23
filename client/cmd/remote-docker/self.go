package main

// Which file this binary is, and how to run it again. Termux runs programs as
// `linker64 <absolute path> <args>` (Android will not exec app data files), and
// a Go binary skips the libc shim that hides it, so:
//   - /proc/self/exe, which os.Executable reads, is the linker
//   - the binary's path arrives as argv[1]
//   - the file cannot be exec'd directly

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// termuxSelfExeEnv is what Termux sets to the path of the running program.
const termuxSelfExeEnv = "TERMUX_EXEC__PROC_SELF_EXE"

// selfPath is this binary's absolute path on disk. The Termux variable wins
// only when os.Executable disagrees and the file exists, so a stale variable
// redirects nothing. Made absolute because the respawn sets its own directory.
func selfPath() (string, error) {
	exe, err := os.Executable()

	hinted := os.Getenv(termuxSelfExeEnv)
	if hinted == "" || (err == nil && sameFile(exe, hinted)) {
		return exe, err
	}
	if _, statErr := os.Stat(hinted); statErr != nil {
		return exe, err
	}
	abs, absErr := filepath.Abs(hinted)
	if absErr != nil {
		return exe, err
	}
	return abs, nil
}

// selfCommand runs this binary again through whatever loader runs this process
// (os.Executable), since Android denies exec of the file itself with
// "permission denied". No linker path is hardcoded; elsewhere this is a plain exec.
func selfCommand(args ...string) (*exec.Cmd, error) {
	self, err := selfPath()
	if err != nil {
		return nil, fmt.Errorf("finding this binary: %w", err)
	}

	if loader, err := os.Executable(); err == nil && !sameFile(loader, self) {
		return exec.Command(loader, append([]string{self}, args...)...), nil
	}
	return exec.Command(self, args...), nil
}

// dropSelfArgument removes an argv[1] that is this binary, which the Termux
// linker inserts (always absolute, so the IsAbs check keeps `docker ps` off the
// filesystem). Deliberately not conditioned on GOOS, so it is exercised everywhere.
func dropSelfArgument(args []string, self string) []string {
	if len(args) < 2 || self == "" || !filepath.IsAbs(args[1]) || !sameFile(args[1], self) {
		return args
	}
	// Capped at 1 so the append allocates rather than writing over args[1].
	return append(args[:1:1], args[2:]...)
}

// sameFile reports whether two paths are one file, following symlinks.
func sameFile(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(ai, bi)
}
