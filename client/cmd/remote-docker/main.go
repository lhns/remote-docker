// Command remote-docker runs Docker on a remote workspace as if it were
// local, with this machine's directories really mounted into the containers.
package main

import (
	"fmt"
	"os"
)

func main() {
	// Before anything reads them: an exec wrapper may have left this binary's
	// own path in the arguments (see self.go). os.Args rather than cobra's
	// SetArgs, because the embedded Docker CLI reads os.Args in its own right
	// and would still see the extra word.
	self, _ := selfPath()
	os.Args = dropSelfArgument(os.Args, self)

	// The error prefix is the name this binary was installed as, because that
	// is the name the user typed. A message beginning "remote-docker:" from a
	// command they spelled `docker` names a program they may not know they
	// have.
	root := newRootCommand()
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, programName()+":", err)
		os.Exit(1)
	}
}
