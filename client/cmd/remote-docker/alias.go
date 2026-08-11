package main

// Answering to a second name.
//
// A machine that cannot install Docker cannot install the docker CLI either,
// and this binary has carried the complete CLI since ADR 0009. It was
// reachable only as
// `remote-docker docker ps`, which is the right thing under the wrong name:
// muscle memory, scripts, IDE integrations and docker's own `context` command
// all look for a plain `docker` on PATH.
//
// So the binary answers to that name too, chosen by the name it was invoked
// by, which is how busybox has worked for thirty years. `shim install` only
// arranges for that name to exist; renaming the downloaded binary to
// docker.exe is a complete installation on its own.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// dockerName is the name that means "this whole command line is docker's".
const dockerName = "docker"

// invokedAsDocker reports whether this process was started under the name
// docker.
//
// os.Args[0], never os.Executable(). The second RESOLVES SYMLINKS: on Linux
// it reads /proc/self/exe, so it reports "remote-docker" for exactly the
// installation this feature creates, and the feature would be silently dead on
// the platform where the symlink is the good answer. argv[0] is the name the
// user typed, which is the question being asked.
func invokedAsDocker() bool { return isDockerName(os.Args[0]) }

// isDockerName reports whether a program path names the docker command.
//
// The extension is stripped and the comparison is case-insensitive because
// Windows is: `DOCKER.EXE` typed at a prompt is the same program, and a path
// that arrived from the shell rather than from a launcher can be spelled
// either way.
func isDockerName(arg0 string) bool {
	name := filepath.Base(arg0)
	name = strings.TrimSuffix(name, filepath.Ext(name))
	return strings.EqualFold(name, dockerName)
}

// termuxSelfExeEnv is where Termux records the path of the running program.
//
// It exists because /proc/self/exe is not that path there. See
// dropSelfArgument.
const termuxSelfExeEnv = "TERMUX_EXEC__PROC_SELF_EXE"

// selfPath is this binary's path on disk.
//
// os.Executable reads /proc/self/exe, and under Termux that is the SYSTEM
// LINKER. A program there is run as `linker64 <absolute path>` to get around
// Android's refusal to execute files in app data directories, so the process
// really is the linker. libtermux-exec patches the answer back up in libc,
// which a Go binary never loads, so os.Executable returns
// /system/bin/linker64 and nothing says so.
//
// It fails at a distance. `start` spawns this binary with the session's
// arguments, so it spawned the LINKER with them, and the log read
//
//	error: expected absolute path: "start"
//
// which is the linker rejecting an argument it was never meant to be given.
// `shim install` would have put a `docker` on PATH that was the linker.
//
// TERMUX_EXEC__PROC_SELF_EXE is what Termux sets to the real path. Preferred
// only where os.Executable disagrees with it and the file is there, so an
// ordinary machine keeps the kernel's answer and a stale variable cannot
// redirect anything.
//
// Always absolute. The variable holds the path as it was typed, so it is
// relative whenever the user typed a relative one, and both callers need it to
// survive a change of directory: the respawn sets the child's Dir, and a shim
// is a link that has to keep resolving from wherever it is used.
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
// Not exec.Command(selfPath()). Android refuses to execute a file in an app
// data directory at all, which is the reason programs there are run as
// `linker64 <absolute path>` in the first place, so a direct respawn is denied
// outright:
//
//	starting the background session: fork/exec .../remote-docker:
//	permission denied
//
// So it re-execs the way it was itself exec'd: through whatever loader is
// running this process. os.Executable IS that loader here, which is why no
// linker path is written down anywhere. Hardcoding /system/bin/linker64 would
// be a guess about a platform nothing tests; this is a measurement, and on a
// machine where the two agree it is an ordinary exec of an ordinary file.
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
// Termux execs a program as `linker64 <absolute path> <args>`, so the path
// lands in the argument vector and `remote-docker status` arrives as
//
//	[<argv0>, /data/data/com.termux/files/home/.../remote-docker, status]
//
// which reads as the program refusing every command it has:
//
//	remote-docker: "/data/data/com.termux/files/home/.../remote-docker"
//	is not a remote-docker command
//
// C programs never see it: the dynamic linker hands main() an argv shifted
// past that entry. Go takes argc and argv off the initial stack instead, so it
// gets the original. That is why `cat` is fine on the same system while this
// is not, and it is measurable -- /proc/self/cmdline shows the extra path for
// EVERY program there, cat included.
//
// Takes several anchors because there is more than one answer to "which file
// am I" here: see selfPath, where os.Executable is the linker and the
// environment variable is the way home. Either matching is enough.
//
// Compared with os.SameFile and not by string, because the inserted path is
// absolute whatever was typed and the two spellings never match.
//
// Not conditioned on GOOS. The test is already narrow enough that it cannot
// fire by accident, an argument has to BE this executable and at position one,
// and a rule that only runs on the platform nobody develops on is a rule that
// rots unnoticed.
func dropSelfArgument(args []string, anchors ...string) []string {
	// Absolute, because the linker requires an absolute path and so that is
	// what gets inserted. It is also what keeps this off the hot path: a
	// subcommand is a bare word, so an ordinary `docker ps` fails here and
	// touches the filesystem not at all.
	if len(args) < 2 || !filepath.IsAbs(args[1]) {
		return args
	}
	for _, anchor := range anchors {
		if anchor != "" && sameFile(args[1], anchor) {
			// Capped at 1 so the append allocates rather than writing over
			// args[1].
			return append(args[:1:1], args[2:]...)
		}
	}
	return args
}
