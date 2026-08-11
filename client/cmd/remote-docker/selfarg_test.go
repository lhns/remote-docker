package main

// An exec wrapper that leaves this binary's path in the arguments.
//
// Reported from Termux, where every command was refused as unknown and the
// word being refused was the binary's own absolute path. Termux execs a program
// as `linker64 <absolute path> <args>`; C programs never see that entry because
// the dynamic linker shifts argv past it, and Go does because it takes argv off
// the initial stack.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// self is this test binary, which is a real file os.Stat can identify.
func self(t *testing.T) string {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Skipf("no executable path on this platform: %v", err)
	}
	return path
}

// termuxArgv is the argument vector as it arrived on the device: the program's
// own absolute path inserted at position one.
func termuxArgv(self string, args ...string) []string {
	return append([]string{"remote-docker", self}, args...)
}

// asMain runs the real command tree the way main does: strip first, then
// execute what is left.
//
// The two anchors are main's two anchors. A test passes the pair it is
// simulating, because which of them Termux leaves pointing at the real file is
// exactly what is not settled.
func asMain(t *testing.T, argv []string, anchors ...string) error {
	t.Helper()
	argv = dropSelfArgument(argv, anchors...)

	root := newTestRoot(t)
	root.SetArgs(argv[1:])
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	return root.Execute()
}

// The bug itself, so that a regression is a failing test rather than a report
// from somebody's phone.
func TestTermuxArgvWouldBreakEveryCommand(t *testing.T) {
	me := self(t)

	// Without the strip, which is what shipped.
	root := newTestRoot(t)
	root.SetArgs(termuxArgv(me, "version")[1:])
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)

	err := root.Execute()
	if err == nil {
		t.Fatal("the unstripped argv was accepted, so this test proves nothing")
	}
	// Quoted the way the message quotes it: %q escapes a Windows path's
	// separators, so the raw string is not in there to be found.
	if !strings.Contains(err.Error(), fmt.Sprintf("%q", me)) {
		t.Fatalf("expected the binary's own path to be the unknown command:\n%v", err)
	}
}

// And with the strip, in both worlds.
//
// Which anchor survives on the device is unknown: /proc/self/exe is the linker
// under this scheme, and libtermux-exec patches that answer back up in libc,
// which a Go binary never loads. So os.Executable may be right or may be the
// linker, and the environment variable is the other way home.
func TestTermuxArgvRunsTheCommand(t *testing.T) {
	me := self(t)

	for _, tc := range []struct {
		name    string
		anchors []string
	}{
		{"os.Executable is correct", []string{me, ""}},
		{"os.Executable is the linker", []string{"/system/bin/linker64", me}},
		{"both anchors give the path", []string{me, me}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, args := range [][]string{
				{"version"},
				{"shim", "status"},
				{"workspace", "list"},
			} {
				if err := asMain(t, termuxArgv(me, args...), tc.anchors...); err != nil {
					t.Errorf("%v under Termux argv: %v", args, err)
				}
			}
		})
	}
}

// A flag after the inserted path used to fail differently, and worse: cobra
// never reached the subcommand, so `shim install --no-path` came back "unknown
// flag: --no-path" and sent the reader after a flag that exists.
func TestTermuxArgvWithAFlagReachesTheSubcommand(t *testing.T) {
	me := self(t)
	// --no-path so nothing is written: the point is that the flag is
	// recognised, and install refuses to touch a docker it did not write.
	err := asMain(t, termuxArgv(me, "shim", "install", "--no-path", "--help"), me)
	if err != nil {
		t.Fatalf("the flag was not reached: %v", err)
	}
}

// Everything else is left exactly alone. This runs on every invocation on every
// platform, so a false positive would eat a real argument.
func TestOrdinaryArgumentsSurvive(t *testing.T) {
	me := self(t)
	for _, args := range [][]string{
		{"remote-docker"},
		{"remote-docker", "status"},
		{"remote-docker", "docker", "run", "--rm", "alpine"},
		// An absolute path that exists and is not us.
		{"remote-docker", filepath.Dir(me), "status"},
		// This binary named at a position that is not the first.
		{"remote-docker", "docker", "run", "-v", me + ":/w", "alpine"},
	} {
		if got := dropSelfArgument(args, me); len(got) != len(args) {
			t.Errorf("%v was rewritten to %v", args, got)
		}
	}
}

// Relative is not the shape this fixes. The linker requires an absolute path,
// so that is what gets inserted, and checking IsAbs first is what keeps an
// ordinary `docker ps` from touching the filesystem at all.
func TestARelativePathIsNotStripped(t *testing.T) {
	me := self(t)
	rel, err := filepath.Rel(filepath.Dir(me), me)
	if err != nil {
		t.Skipf("no relative form available: %v", err)
	}
	t.Chdir(filepath.Dir(me))

	args := []string{"remote-docker", rel, "version"}
	if got := dropSelfArgument(args, me); len(got) != len(args) {
		t.Errorf("a relative argument was stripped: %v", got)
	}
}

// No anchor is not a licence to guess.
func TestWithoutAnAnchorNothingIsStripped(t *testing.T) {
	me := self(t)
	args := termuxArgv(me, "status")

	if got := dropSelfArgument(args); len(got) != len(args) {
		t.Errorf("no anchors rewrote %v", got)
	}
	if got := dropSelfArgument(args, "", ""); len(got) != len(args) {
		t.Errorf("empty anchors rewrote %v", got)
	}
}

// argv[0] is untouched. It is what decides whether this process is the docker
// alias, and it is not ours to rewrite.
func TestArgv0Survives(t *testing.T) {
	me := self(t)
	got := dropSelfArgument([]string{"/system/bin/linker64", me, "status"}, me)
	if got[0] != "/system/bin/linker64" {
		t.Errorf("argv[0] became %q", got[0])
	}
}

// selfPath, which is the other half of the same problem: under Termux
// os.Executable is /system/bin/linker64, so `start` spawned the linker and
// `shim install` would have linked to it.
func TestSelfPath(t *testing.T) {
	me := self(t)

	// An ordinary machine: nothing set, the kernel's answer stands.
	t.Setenv(termuxSelfExeEnv, "")
	got, err := selfPath()
	if err != nil || !sameFile(got, me) {
		t.Errorf("selfPath() = %q, %v; want this binary", got, err)
	}

	// Termux, agreeing with itself: the variable names the same file, so
	// nothing changes hands.
	t.Setenv(termuxSelfExeEnv, me)
	if got, err := selfPath(); err != nil || !sameFile(got, me) {
		t.Errorf("selfPath() = %q, %v; want this binary", got, err)
	}

	// A variable naming a file that is not there is not an answer. It must not
	// displace the one we have.
	t.Setenv(termuxSelfExeEnv, filepath.Join(t.TempDir(), "gone"))
	if got, err := selfPath(); err != nil || !sameFile(got, me) {
		t.Errorf("a missing hinted path was used: %q, %v", got, err)
	}
}

// The case that matters: os.Executable is something else entirely, and the
// variable is the only thing that knows where this binary is.
//
// Simulated by pointing the variable at a DIFFERENT real file, since
// os.Executable cannot be made to lie in a test. If selfPath returns it, it
// preferred the variable over the kernel, which is the behaviour Termux needs.
func TestSelfPathPrefersTheHintWhenTheyDisagree(t *testing.T) {
	other := filepath.Join(t.TempDir(), "linker64")
	if err := os.WriteFile(other, []byte("not really a linker"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(termuxSelfExeEnv, other)

	got, err := selfPath()
	if err != nil {
		t.Fatalf("selfPath: %v", err)
	}
	if !sameFile(got, other) {
		t.Errorf("selfPath() = %q, want the hinted %q", got, other)
	}
}

// The respawn, which is what `start` does and what failed on the device twice
// for two different reasons.
//
// Ordinary machine: this binary, executed directly.
func TestSelfCommandRunsThisBinaryDirectly(t *testing.T) {
	me := self(t)
	t.Setenv(termuxSelfExeEnv, "")

	cmd, err := selfCommand("start", "--foreground")
	if err != nil {
		t.Fatalf("selfCommand: %v", err)
	}
	if !sameFile(cmd.Path, me) {
		t.Errorf("cmd.Path = %q, want this binary", cmd.Path)
	}
	if got := cmd.Args[len(cmd.Args)-2:]; got[0] != "start" || got[1] != "--foreground" {
		t.Errorf("arguments came through as %v", cmd.Args)
	}
}

// Termux: this binary cannot be executed at all, so it goes through the loader
// that is running us, with its own path as the loader's first argument.
//
// Simulated by pointing the variable at a different real file, which is what
// makes selfPath and os.Executable disagree the way they do there.
func TestSelfCommandGoesThroughTheLoaderWhenTheyDisagree(t *testing.T) {
	me := self(t)
	other := filepath.Join(t.TempDir(), "remote-docker")
	if err := os.WriteFile(other, []byte("stand-in"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(termuxSelfExeEnv, other)

	cmd, err := selfCommand("start", "--foreground")
	if err != nil {
		t.Fatalf("selfCommand: %v", err)
	}
	// The loader runs, ...
	if !sameFile(cmd.Path, me) {
		t.Errorf("cmd.Path = %q, want the loader (%q)", cmd.Path, me)
	}
	// ... and what it runs is the first argument after it.
	if len(cmd.Args) < 2 || cmd.Args[1] != other {
		t.Fatalf("the binary is not the loader's first argument: %v", cmd.Args)
	}
	// Absolute, because the loader rejects anything else: `expected absolute
	// path: "start"` is what a relative one produced.
	if !filepath.IsAbs(cmd.Args[1]) {
		t.Errorf("the path handed to the loader is relative: %q", cmd.Args[1])
	}
}

// A relative path in the variable is made absolute. The child's directory is
// changed before it runs, so a relative one would resolve somewhere else, and
// the loader refuses one outright.
func TestSelfPathIsAlwaysAbsolute(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "remote-docker"), []byte("stand-in"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv(termuxSelfExeEnv, "remote-docker")

	got, err := selfPath()
	if err != nil {
		t.Fatalf("selfPath: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("selfPath() = %q, which is relative", got)
	}
}
