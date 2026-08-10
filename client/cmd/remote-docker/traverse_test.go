package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
	if !newRootCommand().TraverseChildren {
		t.Error("the root does not traverse, so flags will be parsed at the deepest command")
	}
	// And the docker command, which IS the root under the `docker` alias.
	if !newDockerCommand().TraverseChildren {
		t.Error("the docker command does not traverse, so the alias will parse flags at the leaf")
	}
}

// Where traversal lands, given a flag halfway down.
func TestMidChainFlagsReachTheirOwnCommand(t *testing.T) {
	// Traversal parses as it walks, and the root's flags write into a package
	// variable, so the case below that uses one is put back afterwards.
	saved := overrides
	t.Cleanup(func() { overrides = saved })

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
			cmd, _, err := newRootCommand().Traverse(tc.args)
			if err != nil {
				t.Fatalf("traversing %v: %v", tc.args, err)
			}
			if cmd.Name() != tc.want {
				t.Errorf("%v landed on %q, want %q", tc.args, cmd.Name(), tc.want)
			}
		})
	}
}

// And the whole way through Execute, which is what actually broke.
//
// `compose config` on a file that is not there needs no daemon: it fails on
// the file. So the error tells us which command got the -f. "no such file"
// means compose parsed it; "unknown shorthand flag" means it was handed to
// `config`, which is the bug.
func TestAMidChainFlagIsNotHandedToTheLeaf(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "absent.yaml")

	root := newRootCommand()
	root.SetArgs([]string{"docker", "compose", "-f", absent, "config"})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)

	err := root.Execute()
	if err == nil {
		t.Fatal("a missing compose file was accepted")
	}
	if strings.Contains(err.Error(), "unknown shorthand flag") {
		t.Fatalf("-f was parsed by `config` rather than by `compose`: %v", err)
	}
	if !strings.Contains(err.Error(), filepath.Base(absent)) {
		t.Errorf("the failure does not name the file, so it may not be the one we expected: %v", err)
	}
}

// The test binary must not be mistaken for the docker alias, or every test
// above would be exercising a different command tree than the one it names.
func TestTheTestBinaryIsNotTheAlias(t *testing.T) {
	if invokedAsDocker() {
		t.Fatalf("os.Args[0] is %q, which reads as the docker alias", os.Args[0])
	}
}
