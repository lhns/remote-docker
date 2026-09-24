package main

import (
	"bytes"
	"context"
	"io"
	"sort"
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

// fakeDaemons stands in for the manager: running is each account's count, an
// account absent from it has no daemon, and reset records what was removed.
type fakeDaemons struct {
	running map[string]int
	reset   []string
}

func (f *fakeDaemons) Accounts(context.Context) ([]string, error) {
	var out []string
	for a := range f.running {
		out = append(out, a)
	}
	sort.Strings(out)
	return out, nil
}

func (f *fakeDaemons) Exists(_ context.Context, a string) bool {
	_, ok := f.running[a]
	return ok
}

func (f *fakeDaemons) Running(_ context.Context, a string) int { return f.running[a] }

func (f *fakeDaemons) Reset(_ context.Context, a string, _ bool) error {
	f.reset = append(f.reset, a)
	return nil
}

func runReset(t *testing.T, f *fakeDaemons, args ...string) (string, error) {
	t.Helper()
	saved := daemonsManager
	daemonsManager = func() (daemonResetter, error) { return f, nil }
	t.Cleanup(func() { daemonsManager = saved })

	root := newRootCommand()
	var out bytes.Buffer
	root.SetArgs(append([]string{"daemons", "reset"}, args...))
	root.SetOut(&out)
	root.SetErr(io.Discard)
	err := root.Execute()
	return out.String(), err
}

func TestDaemonsResetRefusesADaemonRunningContainers(t *testing.T) {
	for _, args := range [][]string{{"alice"}, {"--all"}} {
		f := &fakeDaemons{running: map[string]int{"alice": 2, "bob": 0}}
		_, err := runReset(t, f, args...)
		if err == nil || !strings.Contains(err.Error(), "alice's daemon is running 2 container(s)") ||
			!strings.Contains(err.Error(), "\n  fix: `remote-dockerd daemons reset alice -f`") {
			t.Errorf("reset %v answered %v", args, err)
		}
		if len(f.reset) != 0 {
			t.Errorf("reset %v refused but removed %v", args, f.reset)
		}
	}

	for _, args := range [][]string{{"alice", "-f"}, {"--all", "--force"}} {
		f := &fakeDaemons{running: map[string]int{"alice": 2, "bob": -1}}
		if _, err := runReset(t, f, args...); err != nil || len(f.reset) == 0 {
			t.Errorf("reset %v answered %v and removed %v", args, err, f.reset)
		}
	}
}

func TestDaemonsResetOfNoDaemonSaysSo(t *testing.T) {
	f := &fakeDaemons{running: map[string]int{}}
	out, err := runReset(t, f, "carol")
	if err != nil || out != "no daemon for carol\n" || len(f.reset) != 0 {
		t.Errorf("reset carol printed %q, answered %v, removed %v", out, err, f.reset)
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
