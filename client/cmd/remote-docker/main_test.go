package main

// Helpers the tests in this package share.

import (
	"os"
	"testing"

	"github.com/spf13/cobra"
)

// newTestRoot builds the real command tree, docker and compose included.
//
// Per call rather than shared: cobra commands hold the flags they parsed, so a
// root reused across cases would carry the previous one's state into the next.
func newTestRoot(t *testing.T) *cobra.Command {
	t.Helper()

	// The root's flags write into a package variable, so anything that parses
	// one has to put it back.
	saved := overrides
	t.Cleanup(func() { overrides = saved })

	return newRootCommand()
}

// withArgs replaces os.Args for one test. Everything that reads a command line
// before cobra parses it reads os.Args directly, so that is what has to move.
func withArgs(t *testing.T, args []string) {
	t.Helper()
	old := os.Args
	os.Args = args
	t.Cleanup(func() { os.Args = old })
}
