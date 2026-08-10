package main

// Answering to a second name.
//
// A machine that cannot install Docker cannot install the docker CLI either --
// on Windows every route to one leads to Docker Desktop -- and this binary has
// carried the complete CLI since ADR 0009. It was reachable only as
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
// os.Args[0], never os.Executable(). The second RESOLVES SYMLINKS -- on Linux
// it reads /proc/self/exe -- so it reports "remote-docker" for exactly the
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
