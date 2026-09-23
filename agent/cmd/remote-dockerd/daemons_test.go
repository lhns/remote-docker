package main

import (
	"io"
	"strings"
	"testing"
)

func TestDaemonsRefusesAnUnknownSubcommand(t *testing.T) {
	root := newRootCommand()
	root.SetArgs([]string{"daemons", "bogus"})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), `"bogus"`) {
		t.Errorf("daemons bogus answered %v, want an error naming the word", err)
	}
}

func TestDaemonsListIsAnAliasForLs(t *testing.T) {
	cmd, _, err := newRootCommand().Find([]string{"daemons", "list"})
	if err != nil || cmd.Name() != "ls" {
		t.Errorf("daemons list found %v (%v), want ls", cmd.Name(), err)
	}
}

func TestDaemonsResetWithoutAnAccountNamesTheRemedy(t *testing.T) {
	root := newRootCommand()
	root.SetArgs([]string{"daemons", "reset"})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "\n  fix: ") {
		t.Errorf("daemons reset answered %v, want a fix line", err)
	}
}
