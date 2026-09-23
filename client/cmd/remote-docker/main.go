// Command remote-docker runs Docker on a remote workspace as if it were
// local, with this machine's directories really mounted into the containers.
package main

import (
	"errors"
	"fmt"
	"os"
	"runtime"

	"github.com/docker/cli/cli"
)

func main() {
	// Both repairs rewrite os.Args, not cobra's SetArgs: the embedded Docker CLI
	// reads os.Args itself. self.go explains the first.
	self, _ := selfPath()
	os.Args = dropSelfArgument(os.Args, self)

	// Git Bash mangles the container side of `-v` (ADR 0040).
	if runtime.GOOS == "windows" {
		var notes []string
		os.Args, notes = msysFrom(os.Getenv).repairArgs(os.Args)
		for _, note := range notes {
			fmt.Fprintf(os.Stderr, "%s: Git Bash rewrote a -v argument; %s\n", programName(), note)
		}
	}

	root := newRootCommand()
	err := root.Execute()
	if err == nil {
		return
	}

	// A container's non-zero exit arrives as a cli.StatusError whose message is
	// "", which must print nothing.
	if msg := err.Error(); msg != "" {
		fmt.Fprintln(os.Stderr, programName()+":", msg)
	}
	os.Exit(exitCode(err))
}

// exitCode passes a container's status through, as the Docker CLI does.
// Docker's 128+signal mapping is not needed: an interrupted `docker run` gets
// its status from the container too (test/integration.sh section 6e).
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	// A zero StatusCode on an error would turn the failure into a success.
	var status cli.StatusError
	if errors.As(err, &status) && status.StatusCode != 0 {
		return status.StatusCode
	}
	return 1
}
