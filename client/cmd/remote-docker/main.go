// Command remote-docker runs Docker on a remote workspace as if it were
// local, with this machine's directories really mounted into the containers.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/docker/cli/cli"
)

func main() {
	// Before anything reads them: an exec wrapper may have left this binary's
	// own path in the arguments (see self.go). os.Args rather than cobra's
	// SetArgs, because the embedded Docker CLI reads os.Args in its own right
	// and would still see the extra word.
	self, _ := selfPath()
	os.Args = dropSelfArgument(os.Args, self)

	root := newRootCommand()
	err := root.Execute()
	if err == nil {
		return
	}

	// An EMPTY message means print nothing. That is not defensiveness: it is
	// how the Docker CLI returns a container's exit status. `docker run`
	// finishing returns cli.StatusError with only a StatusCode, and its
	// Error() is deliberately "" -- so printing unconditionally puts a bare
	// "remote-docker:" on the terminal after every container that exits
	// non-zero.
	//
	// The prefix is the name this binary was installed as, because that is the
	// name the user typed. A message beginning "remote-docker:" from a command
	// they spelled `docker` names a program they may not know they have.
	if msg := err.Error(); msg != "" {
		fmt.Fprintln(os.Stderr, programName()+":", msg)
	}
	os.Exit(exitCode(err))
}

// exitCode is the status this process exits with for err.
//
// A container's exit code reaches here as cli.StatusError and must be passed
// through: this binary IS the Docker CLI (ADR 0024), so `docker run ... ; echo
// $?` has to answer what the container answered. Collapsing everything to 1
// breaks any script that branches on it.
//
// One case is deliberately NOT handled. Docker maps a signal-terminated
// context to 128+signal, but the error it uses for that is unexported in its
// own package main, so it cannot be recognised from here without matching on
// message text. Ctrl-C therefore exits 1 rather than 130.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	// Zero is excluded because it is not an exit status a failure can have:
	// docker sets StatusCode only when it means it, and a zero would turn a
	// real error into a success.
	var status cli.StatusError
	if errors.As(err, &status) && status.StatusCode != 0 {
		return status.StatusCode
	}
	return 1
}
