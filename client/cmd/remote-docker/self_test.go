package main

// Reported from Termux: every command refused as unknown, the refused word
// being the binary's own absolute path, and then `start` unable to launch
// anything. See self.go for why.
//
// os.Executable cannot be made to lie in a test, so the device is simulated
// from the other side: the environment variable points at a different real
// file, which is the same disagreement Termux produces.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// self is this test binary, a real file os.Stat can identify.
func self(t *testing.T) string {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Skipf("no executable path on this platform: %v", err)
	}
	return path
}

// standIn is another real file, standing in for whichever of the two paths the
// test needs to be wrong.
func standIn(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("stand-in"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// termuxArgv is the argument vector as it arrived on the device: the program's
// own absolute path inserted at position one.
func termuxArgv(self string, args ...string) []string {
	return append([]string{"remote-docker", self}, args...)
}

func TestSelfPath(t *testing.T) {
	me := self(t)

	t.Run("nothing set, the kernel answers", func(t *testing.T) {
		t.Setenv(termuxSelfExeEnv, "")
		if got, err := selfPath(); err != nil || !sameFile(got, me) {
			t.Errorf("selfPath() = %q, %v; want this binary", got, err)
		}
	})

	t.Run("agreeing, nothing changes hands", func(t *testing.T) {
		t.Setenv(termuxSelfExeEnv, me)
		if got, err := selfPath(); err != nil || !sameFile(got, me) {
			t.Errorf("selfPath() = %q, %v; want this binary", got, err)
		}
	})

	// A variable naming a file that is not there is not an answer, and must not
	// displace the one we have.
	t.Run("a hint that names nothing is ignored", func(t *testing.T) {
		t.Setenv(termuxSelfExeEnv, filepath.Join(t.TempDir(), "gone"))
		if got, err := selfPath(); err != nil || !sameFile(got, me) {
			t.Errorf("selfPath() = %q, %v; want this binary", got, err)
		}
	})

	// The case that matters: os.Executable is the linker, and the variable is
	// the only thing that knows where this binary is.
	t.Run("disagreeing, the hint wins", func(t *testing.T) {
		other := standIn(t, "remote-docker")
		t.Setenv(termuxSelfExeEnv, other)
		got, err := selfPath()
		if err != nil || !sameFile(got, other) {
			t.Errorf("selfPath() = %q, %v; want the hinted %q", got, err, other)
		}
	})

	// The variable holds the path as typed. The respawn changes the child's
	// directory and the loader refuses a relative path outright.
	t.Run("a relative hint is made absolute", func(t *testing.T) {
		other := standIn(t, "remote-docker")
		t.Chdir(filepath.Dir(other))
		t.Setenv(termuxSelfExeEnv, "remote-docker")

		got, err := selfPath()
		if err != nil {
			t.Fatalf("selfPath: %v", err)
		}
		if !filepath.IsAbs(got) {
			t.Errorf("selfPath() = %q, which is relative", got)
		}
	})
}

// The respawn, which is what `start` does and what failed on the device twice
// for two different reasons.
func TestSelfCommand(t *testing.T) {
	me := self(t)

	t.Run("an ordinary machine execs the file", func(t *testing.T) {
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
	})

	// Termux: the file cannot be executed, so it goes through the loader that
	// is running us, with its own path as the loader's first argument.
	t.Run("otherwise it goes through the loader", func(t *testing.T) {
		other := standIn(t, "remote-docker")
		t.Setenv(termuxSelfExeEnv, other)

		cmd, err := selfCommand("start", "--foreground")
		if err != nil {
			t.Fatalf("selfCommand: %v", err)
		}
		if !sameFile(cmd.Path, me) {
			t.Errorf("cmd.Path = %q, want the loader (%q)", cmd.Path, me)
		}
		if len(cmd.Args) < 2 || cmd.Args[1] != other {
			t.Fatalf("the binary is not the loader's first argument: %v", cmd.Args)
		}
		// `expected absolute path: "start"` is what a relative one produced.
		if !filepath.IsAbs(cmd.Args[1]) {
			t.Errorf("the path handed to the loader is relative: %q", cmd.Args[1])
		}
	})
}

func TestDropSelfArgument(t *testing.T) {
	me := self(t)
	rel, relErr := filepath.Rel(filepath.Dir(me), me)

	for _, tc := range []struct {
		name    string
		args    []string
		self    string
		dropped bool
	}{
		{"the reported shape", termuxArgv(me, "status", "--workspace", "dev"), me, true},
		// This runs on every invocation on every platform, so a false positive
		// would eat a real argument.
		{"no arguments", []string{"remote-docker"}, me, false},
		{"an ordinary subcommand", []string{"remote-docker", "status"}, me, false},
		{"a docker command", []string{"remote-docker", "docker", "run", "--rm", "alpine"}, me, false},
		{"an absolute path that is not us", []string{"remote-docker", filepath.Dir(me), "status"}, me, false},
		{"us, but not at position one", []string{"remote-docker", "docker", "run", "-v", me + ":/w"}, me, false},
		// The linker requires an absolute path, so that is what gets inserted,
		// and checking that first is what keeps `docker ps` off the filesystem.
		{"a relative spelling", []string{"remote-docker", rel, "status"}, me, false},
		{"no anchor is no licence to guess", termuxArgv(me, "status"), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if strings.Contains(tc.name, "relative") {
				if relErr != nil {
					t.Skipf("no relative form available: %v", relErr)
				}
				t.Chdir(filepath.Dir(me))
			}

			got := dropSelfArgument(tc.args, tc.self)
			want := len(tc.args)
			if tc.dropped {
				want--
			}
			if len(got) != want {
				t.Fatalf("dropSelfArgument(%v) = %v", tc.args, got)
			}
			// argv[0] is never touched: it is what decides whether this process
			// is the docker alias, and it is not ours to rewrite.
			if got[0] != tc.args[0] {
				t.Errorf("argv[0] became %q", got[0])
			}
		})
	}
}

// asMain runs the real command tree the way main does: strip first, then
// execute what is left.
func asMain(t *testing.T, argv []string, self string) error {
	t.Helper()
	argv = dropSelfArgument(argv, self)

	root := newTestRoot(t)
	root.SetArgs(argv[1:])
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	return root.Execute()
}

// The bug itself, so a regression is a failing test rather than a report from
// somebody's phone.
func TestTermuxArgvWouldBreakEveryCommand(t *testing.T) {
	me := self(t)

	err := asMain(t, termuxArgv(me, "version"), "")
	if err == nil {
		t.Fatal("the unstripped argv was accepted, so this test proves nothing")
	}
	// Quoted the way the message quotes it: %q escapes a Windows path's
	// separators, so the raw string is not in there to be found.
	if !strings.Contains(err.Error(), fmt.Sprintf("%q", me)) {
		t.Fatalf("expected the binary's own path to be the unknown command:\n%v", err)
	}
}

func TestTermuxArgvRunsTheCommand(t *testing.T) {
	me := self(t)
	for _, args := range [][]string{
		{"version"},
		{"shim", "status"},
		{"workspace", "list"},
		// A flag after the inserted path failed differently and worse: cobra
		// never reached the subcommand, so this came back "unknown flag:
		// --no-path" and sent the reader after a flag that exists.
		{"shim", "install", "--no-path", "--help"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			if err := asMain(t, termuxArgv(me, args...), me); err != nil {
				t.Errorf("%v under Termux argv: %v", args, err)
			}
		})
	}
}
