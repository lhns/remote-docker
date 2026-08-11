package main

// What the embedded Compose can be asked without a daemon.
//
// Its own file because there will be more of these: compose is a dependency
// now (ADR 0009), it moves on its own release cadence, and the pairing between
// it, buildx, docker/cli and buildkit is ours to keep working. The integration
// suite proves compose can bring a stack up on a real workspace; these prove
// the parts that need nothing but a compose file, in a second rather than a
// minute.

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// composeFile writes a project and returns its path.
func composeFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "compose.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing the compose file: %v", err)
	}
	return path
}

// `compose config` resolves a project and prints it, which needs no daemon and
// is the cheapest proof that the embedded Compose is wired up and working.
func TestComposeResolvesAProject(t *testing.T) {
	file := composeFile(t, `
services:
  web:
    image: nginx:alpine
    ports:
      - "8080:80"
`)

	var err error
	out := captureStdout(t, func() {
		root := newTestRoot(t)
		root.SetArgs([]string{"compose", "-f", file, "config"})
		err = root.Execute()
	})

	if err != nil {
		t.Fatalf("compose config: %v\n%s", err, out)
	}
	for _, want := range []string{"web", "nginx:alpine", "8080"} {
		if !strings.Contains(out, want) {
			t.Errorf("the resolved project does not mention %q:\n%s", want, out)
		}
	}
}

// captureStdout runs fn with the process's stdout redirected, and returns what
// was written to it.
//
// Cobra's SetOut is not enough. The embedded CLI writes through
// dockerCli.Out(), which binds to os.Stdout when the CLI is CONSTRUCTED, so
// the swap has to happen before the command tree is built, which is why fn
// builds it. Everything docker and compose print goes to the process's
// streams; only our own commands honour cobra's.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	saved := os.Stdout
	os.Stdout = w

	read := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		read <- string(b)
	}()

	fn()

	os.Stdout = saved
	_ = w.Close()
	out := <-read
	_ = r.Close()
	return out
}

// The flag halfway down the chain, all the way through Execute.
//
// `-f` belongs to `compose`, and `config` comes after it. Cobra decides
// traversal at the root, so this is the case that broke: the flag was handed
// to `config`, which has never heard of it.
//
// A file that is not there is the probe, because it fails in the loader rather
// than anywhere near a daemon. The assertion is on the error's IDENTITY, not
// its wording: fs.ErrNotExist means compose parsed the flag and went looking
// for the file, and compose is free to reword itself without breaking this.
func TestAMidChainFlagIsNotHandedToTheLeaf(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "absent.yaml")

	root := newTestRoot(t)
	root.SetArgs([]string{"compose", "-f", absent, "config"})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)

	err := root.Execute()
	if err == nil {
		t.Fatal("a missing compose file was accepted")
	}
	// A flag-parsing failure is not a missing file, so this one assertion
	// covers both: "unknown shorthand flag: 'f'" fails it.
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("wanted the missing file, got: %v", err)
	}
}
