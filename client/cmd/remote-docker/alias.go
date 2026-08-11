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
	"os"
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
// Two anchors because it is not settled which one Termux leaves intact.
// /proc/self/exe is the linker under this scheme, and libtermux-exec patches
// the answer back up in libc, which a Go binary never loads. So os.Executable
// may report the linker, and the environment variable is what Termux sets to
// the real path. Either matching is enough; both being wrong changes nothing.
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
