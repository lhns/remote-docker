package main

// Which file this binary is, and how to run it again.
//
// Both are ordinary questions everywhere except Termux, where Android refuses
// to execute files in app data directories at all. Programs there are run as
// `linker64 <absolute path> <args>`, so the process really IS the system
// linker. libtermux-exec papers over that in libc, and a Go binary loads
// neither, so it sees the arrangement raw:
//
//   - /proc/self/exe, which os.Executable reads, is the linker
//   - the binary's path arrives as an argument, because Go takes argv off the
//     initial stack rather than from the linker, which shifts past it for C
//   - the file cannot be exec'd directly, whatever path it is given
//
// Each one failed somewhere else entirely, naming the linker or nothing.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// termuxSelfExeEnv is what Termux sets to the path of the running program.
const termuxSelfExeEnv = "TERMUX_EXEC__PROC_SELF_EXE"

// selfPath is this binary's path on disk.
//
// The environment variable is preferred only where os.Executable disagrees
// with it and the file is really there, so an ordinary machine keeps the
// kernel's answer and a stale variable cannot redirect anything.
//
// Always absolute: the variable holds the path as it was typed. `shim install`
// makes a link that has to resolve from elsewhere, and the respawn below sets
// the child's directory, so a relative path would find nothing there.
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

// selfCommand builds a command that runs this binary again.
//
// Not exec.Command(selfPath()), which `start` used and which Android denies
// outright:
//
//	starting the background session: fork/exec .../remote-docker:
//	permission denied
//
// So it re-execs the way it was itself exec'd: through whatever loader is
// running this process, which os.Executable names. That is why no linker path
// appears anywhere here -- hardcoding /system/bin/linker64 would be a guess
// about a platform nothing tests. Where the two agree this is an ordinary exec
// of an ordinary file.
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

// dropSelfArgument removes a first argument that is this binary.
//
// `remote-docker status` arrives as [<argv0>, /abs/path/remote-docker, status],
// so the first word the CLI reads as a command is its own path and every
// command is refused as unknown:
//
//	remote-docker: "/data/data/com.termux/files/home/.../remote-docker"
//	is not a remote-docker command
//
// Absolute first, because the linker requires an absolute path and so that is
// what gets inserted. It is also what keeps this off the hot path: a
// subcommand is a bare word, so an ordinary `docker ps` never reaches the
// filesystem. os.SameFile after it, because the inserted path is absolute
// whatever was typed and the spellings would not match.
//
// Not conditioned on GOOS. An argument has to BE this executable and at
// position one, which cannot happen by accident, and a rule that runs only on
// the platform nobody develops on is a rule that rots.
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
