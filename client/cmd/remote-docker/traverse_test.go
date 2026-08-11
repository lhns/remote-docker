package main

import (
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

// A flag belongs to the command that declares it, wherever that command sits
// in the chain. `--context` is the root's; `-f` is compose's; both have
// subcommands after them.
//
// Cobra decides this at the ROOT and for the whole tree: ExecuteC calls Find()
// unless the root sets TraverseChildren, and Find parses every flag at the
// deepest command it lands on.
//
// The claim lived in a comment for three days and was wrong the whole time,
// because nothing ran it. This runs it.
func TestTheRootTraverses(t *testing.T) {
	if !newTestRoot(t).TraverseChildren {
		t.Error("the root does not traverse, so flags will be parsed at the deepest command")
	}
}

// Where traversal lands, given a flag halfway down.
func TestMidChainFlagsReachTheirOwnCommand(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"docker's own flag", []string{"--context", "dev", "ps"}, "ps"},
		{"docker's shorthand", []string{"-D", "ps"}, "ps"},
		{"compose's file flag", []string{"compose", "-f", "x.yaml", "up"}, "up"},
		{"compose's project flag", []string{"compose", "-p", "proj", "ls"}, "ls"},
		{"ours, under remote", []string{"remote", "--workspace", "dev", "status"}, "status"},
		{"no flags at all", []string{"compose", "ls"}, "ls"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd, _, err := newTestRoot(t).Traverse(tc.args)
			if err != nil {
				t.Fatalf("traversing %v: %v", tc.args, err)
			}
			if cmd.Name() != tc.want {
				t.Errorf("%v landed on %q, want %q", tc.args, cmd.Name(), tc.want)
			}
		})
	}
}
