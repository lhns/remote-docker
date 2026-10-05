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
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docker/cli/cli/command"
	cliflags "github.com/docker/cli/cli/flags"
	"github.com/spf13/cobra"
)

// composeTree is the compose command alone, asking client for the run's id.
// Built inside captureStdout's fn: the CLI binds stdout when constructed.
func composeTree(t *testing.T, client func() string) *cobra.Command {
	t.Helper()
	dockerCli, err := command.NewDockerCli()
	if err != nil {
		t.Fatalf("docker cli: %v", err)
	}
	if err := dockerCli.Initialize(cliflags.NewClientOptions()); err != nil {
		t.Fatalf("initialising the docker cli: %v", err)
	}
	root := &cobra.Command{Use: "docker", TraverseChildren: true, SilenceUsage: true, SilenceErrors: true}
	installCompose(root, dockerCli, client)
	return root
}

// projectNameOf runs `compose config` over a project in a directory named
// app, its file starting with body, and returns the name compose resolved.
func projectNameOf(t *testing.T, body string, client func() string, flags ...string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "app")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(file, []byte(body+"services:\n  web:\n    image: nginx:alpine\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var err error
	out := captureStdout(t, func() {
		root := composeTree(t, client)
		args := append([]string{"compose", "-f", file}, flags...)
		root.SetArgs(append(args, "config", "--format", "json"))
		err = root.Execute()
	})
	if err != nil {
		t.Fatalf("compose config: %v\n%s", err, out)
	}
	var project struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(out), &project); err != nil {
		t.Fatalf("decoding the resolved project: %v\n%s", err, out)
	}
	return project.Name
}

// An ephemeral run names its projects after itself, so two runs of one account
// bringing up one file are two projects (ADR 0050).
func TestComposeNamesAnEphemeralRunsProject(t *testing.T) {
	t.Setenv("COMPOSE_PROJECT_NAME", "")
	if got := projectNameOf(t, "", func() string { return "c1" }); got != "app-c1" {
		t.Errorf("project = %q, want app-c1", got)
	}
	// A file's own name is the base, as the directory's is without one.
	if got := projectNameOf(t, "name: shop\n", func() string { return "c1" }); got != "shop-c1" {
		t.Errorf("named file: project = %q, want shop-c1", got)
	}
}

func TestComposeKeepsAnExplicitProjectName(t *testing.T) {
	run := func() string { return "c1" }

	t.Setenv("COMPOSE_PROJECT_NAME", "")
	if got := projectNameOf(t, "", run, "-p", "mine"); got != "mine" {
		t.Errorf("with -p: project = %q, want mine", got)
	}

	t.Setenv("COMPOSE_PROJECT_NAME", "fromenv")
	if got := projectNameOf(t, "", run); got != "fromenv" {
		t.Errorf("with COMPOSE_PROJECT_NAME: project = %q, want fromenv", got)
	}
}

// A machine has no run id, and an invocation that is not ours is not asked.
func TestComposeLeavesAMachinesProjectName(t *testing.T) {
	t.Setenv("COMPOSE_PROJECT_NAME", "")
	if got := projectNameOf(t, "", func() string { return "" }); got != "app" {
		t.Errorf("machine: project = %q, want app", got)
	}
	if got := projectNameOf(t, "", nil); got != "app" {
		t.Errorf("not ours: project = %q, want app", got)
	}
}

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
