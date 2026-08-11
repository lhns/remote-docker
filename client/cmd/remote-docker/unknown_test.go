package main

// A word that is not a command must fail, and say so.
//
// It used to print the help and exit 0 at every level of the tree, which is
// how a whole session of mistyped commands on a bad terminal looked like a
// program that would not do anything.

import (
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// run executes the real command tree and returns the error, with both streams
// discarded so a help screen does not land in the test output.
func run(t *testing.T, args ...string) error {
	t.Helper()
	root := newTestRoot(t)
	root.SetArgs(args)
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	return root.Execute()
}

func TestAnUnknownCommandIsAnError(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		// what the message has to contain: the word the user typed, and the
		// command path it was not a command of.
		word string
		path string
	}{
		{"at the root", []string{"bogus"}, `"bogus"`, "remote-docker"},
		{"under workspace", []string{"workspace", "bogus"}, `"bogus"`, "remote-docker workspace"},
		{"under shim", []string{"shim", "bogus"}, `"bogus"`, "remote-docker shim"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := run(t, tc.args...)
			if err == nil {
				t.Fatalf("%v was accepted", tc.args)
			}
			for _, want := range []string{tc.word, tc.path} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the error does not mention %s:\n%v", want, err)
				}
			}
		})
	}
}

// The typo is the case this exists for, so the nearest command is named rather
// than the whole list.
//
// Both kinds of near miss: `creat` shares a prefix with `create`, `statuss`
// does not share one with `status` and is reachable only by edit distance.
// Unset, cobra's distance is 0 and finds the first and not the second.
func TestANearMissNamesTheCommandYouMeant(t *testing.T) {
	for _, tc := range []struct {
		args   []string
		expect string
	}{
		{[]string{"statuss"}, "remote-docker status"},
		{[]string{"workspace", "creat"}, "remote-docker workspace create"},
		{[]string{"shim", "instal"}, "remote-docker shim install"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			err := run(t, tc.args...)
			if err == nil {
				t.Fatalf("%v was accepted", tc.args)
			}
			if !strings.Contains(err.Error(), tc.expect) {
				t.Errorf("no suggestion of %q:\n%v", tc.expect, err)
			}
		})
	}
}

// The worst version of the bug: `workspace` lists when given no subcommand, so
// a misspelled `create` fell through to the parent, listed the workspaces and
// exited 0 having created nothing.
func TestAMisspelledSubcommandDoesNotRunItsParent(t *testing.T) {
	err := run(t, "workspace", "creat", "dev")
	if err == nil {
		t.Fatal("`workspace creat dev` succeeded, which means it ran the list")
	}
}

// And the commands that legitimately take no arguments still work, which is
// what the Args rule must not break.
func TestBareCommandsStillRun(t *testing.T) {
	for _, args := range [][]string{
		{},          // help, and exit 0
		{"shim"},    // help, and exit 0
		{"version"}, //
		{"shim", "status"},
	} {
		t.Run(strings.Join(append([]string{"remote-docker"}, args...), " "), func(t *testing.T) {
			if err := run(t, args...); err != nil {
				t.Errorf("%v failed: %v", args, err)
			}
		})
	}
}

// The rule itself, away from the tree: no arguments is not an error, because
// that is how a bare `workspace` reaches its own RunE.
func TestOnlySubcommandsAcceptsNoArguments(t *testing.T) {
	cmd := &cobra.Command{Use: "parent"}
	if err := onlySubcommands(cmd, nil); err != nil {
		t.Errorf("no arguments was refused: %v", err)
	}
	if err := onlySubcommands(cmd, []string{"child"}); err == nil {
		t.Error("an argument was accepted by a command with no subcommands")
	}
}
