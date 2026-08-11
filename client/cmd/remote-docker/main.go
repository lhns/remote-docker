// Command remote-docker runs Docker on a remote workspace as if it were
// local, with this machine's directories really mounted into the containers.
package main

import (
	"fmt"
	"os"
)

func main() {
	// Before anything reads them: an exec wrapper may have left this binary's
	// own path in the arguments. See dropSelfArgument. os.Args rather than
	// cobra's SetArgs, because the embedded Docker CLI reads os.Args in its own
	// right and would still see the extra word.
	self, _ := selfPath()
	os.Args = dropSelfArgument(os.Args, self, os.Getenv(termuxSelfExeEnv))

	// Under the name `docker` the whole command line belongs to the Docker
	// CLI, and so does the help: `docker run --help` must not describe itself
	// as a subcommand of something else. The error prefix follows the name for
	// the same reason: a message beginning "remote-docker:" from a command
	// the user spelled `docker` names a program they may not know they have.
	name, root := "remote-docker", newRootCommand()
	if invokedAsDocker() {
		name, root = dockerName, newDockerCommand()
	}

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, name+":", err)
		os.Exit(1)
	}
}
