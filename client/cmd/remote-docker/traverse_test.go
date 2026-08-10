package main

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

// A flag belongs to the command that declares it, wherever that command sits
// in the chain. `--context` is the docker command's; `-f` is compose's; both
// have subcommands after them.
//
// Cobra decides this at the ROOT and for the whole tree: ExecuteC calls Find()
// unless the root sets TraverseChildren, and Find parses every flag at the
// deepest command it lands on. Upstream sets it on its own root; we set it on
// the docker command, copied from there, where it did nothing on this path.
//
// The claim lived in a comment for three days and was wrong the whole time,
// because nothing ran it. This runs it.
func TestTheRootTraverses(t *testing.T) {
	if !newTestRoot(t).TraverseChildren {
		t.Error("the root does not traverse, so flags will be parsed at the deepest command")
	}
	// And the docker command, which IS the root under the `docker` alias.
	if !newDockerCommand().TraverseChildren {
		t.Error("the docker command does not traverse, so the alias will parse flags at the leaf")
	}
}

// Where traversal lands, given a flag halfway down.
func TestMidChainFlagsReachTheirOwnCommand(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"docker's own flag", []string{"docker", "--context", "dev", "ps"}, "ps"},
		{"docker's shorthand", []string{"docker", "-D", "ps"}, "ps"},
		{"compose's file flag", []string{"docker", "compose", "-f", "x.yaml", "up"}, "up"},
		{"compose's project flag", []string{"docker", "compose", "-p", "proj", "ls"}, "ls"},
		{"a root flag first", []string{"--workspace", "dev", "status"}, "status"},
		{"no flags at all", []string{"docker", "compose", "ls"}, "ls"},
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

// The test binary must not be mistaken for the docker alias, or every test
// above would be exercising a different command tree than the one it names.
func TestTheTestBinaryIsNotTheAlias(t *testing.T) {
	if invokedAsDocker() {
		t.Fatalf("os.Args[0] is %q, which reads as the docker alias", os.Args[0])
	}
}
