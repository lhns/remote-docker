// Command remote-docker runs Docker on a remote workspace as if it were
// local, with this machine's directories really mounted into the containers.
package main

import (
	"fmt"
	"os"
)

func main() {
	// After Execute, not deferred inside the docker command: a session started
	// implicitly for `remote-docker docker ...` has to outlive the command
	// that is using it.
	defer closeImplicitSession()

	if err := newRootCommand().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "remote-docker:", err)
		closeImplicitSession()
		os.Exit(1)
	}
}
