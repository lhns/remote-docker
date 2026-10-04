package main

// Removing a workspace that has a machine behind it.
//
// The config entry is the only record that a Linux system was ever built for a
// workspace. So the rule is: destroy the machine first, and if that cannot be
// done, refuse — because deleting the entry anyway leaves a machine running on
// somebody's laptop with nothing on the system naming it.
//
// On a platform with no backend compiled in, which is every platform this is
// developed on, that refusal is exactly what happens and is what this pins.

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lhns/remote-docker/client/internal/config"
	"github.com/lhns/remote-docker/machine"
)

func TestRemovingAMachineWithNoBackendRefuses(t *testing.T) {
	if len(machine.Backends()) > 0 {
		t.Skip("this platform has a backend, so the refusal is not reachable here")
	}

	root := newTestRoot(t)
	err := destroyMachine(root, &config.Machine{Backend: "wsl", Name: "rd-dev"})
	if err == nil {
		t.Fatal("the machine was reported destroyed by a build that cannot destroy it")
	}

	// The name and the backend, because the user now has to deal with it by
	// hand and needs to know what to look for.
	for _, want := range []string{"rd-dev", "wsl"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %q:\n%v", want, err)
		}
	}
}

// A workspace with no machine is not a machine with no backend. Nothing should
// be attempted, and nothing should fail.
func TestAWorkspaceWithoutAMachineNeedsNoBackend(t *testing.T) {
	file := config.File{
		Workspaces: map[string]config.Workspace{
			"plain":   {Host: "box.example"},
			"machine": {Host: "127.0.0.1", Machine: &config.Machine{Backend: "wsl", Name: "rd-dev"}},
		},
	}

	if file.Workspaces["plain"].Machine != nil {
		t.Error("an ordinary workspace reported a machine")
	}
	if file.Workspaces["machine"].Machine == nil {
		t.Fatal("a machine-backed workspace lost its machine through the config round trip")
	}
	if got := file.Workspaces["machine"].Machine.Name; got != "rd-dev" {
		t.Errorf("machine name = %q, want rd-dev", got)
	}
}

// A flag left unset falls back to what the machine was recorded as built from,
// so `machine rebuild` builds the same machine `machine create --cpus 4` did
// and its generation matches the record again.
func TestSpecFallsBackToTheRecordedMachine(t *testing.T) {
	saved := overrides
	t.Cleanup(func() { overrides = saved })
	overrides = config.Overrides{}

	image := machine.DefaultImage(version)
	rootfs := filepath.Join(t.TempDir(), "rootfs.tar")
	if err := os.WriteFile(rootfs, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	recorded := &config.Workspace{
		Port: 2222, User: "alice",
		Machine: &config.Machine{Backend: "wsl", Name: "dev", Image: image, Rootfs: rootfs, CPUs: 4, MemoryMB: 8192},
	}

	unset := mustSpec(t, &machineOptions{backend: "wsl"}, "dev", recorded)
	if unset.CPUs != 4 || unset.MemoryMB != 8192 || unset.Rootfs != rootfs {
		t.Errorf("spec with no flags = cpus %d, memory %d, rootfs %q; want the recorded 4, 8192, %s",
			unset.CPUs, unset.MemoryMB, unset.Rootfs, rootfs)
	}
	// The port and the account are in the generation too: without the
	// fallback a rebuild of a machine created with --port 2222 destroys it
	// and builds one on 22.
	if unset.Port != 2222 || unset.Account != "alice" {
		t.Errorf("spec with no flags = port %d, account %q; want the recorded 2222, alice", unset.Port, unset.Account)
	}

	set := mustSpec(t, &machineOptions{backend: "wsl", cpus: 2, memoryMB: 1024, rootfs: "/mine.tar"}, "dev", recorded)
	if set.CPUs != 2 || set.MemoryMB != 1024 || set.Rootfs != "/mine.tar" {
		t.Errorf("spec with flags = cpus %d, memory %d, rootfs %q; want the flags to win",
			set.CPUs, set.MemoryMB, set.Rootfs)
	}
	overrides = config.Overrides{Port: 2200, User: "Bob"}
	if got := mustSpec(t, &machineOptions{backend: "wsl"}, "dev", recorded); got.Port != 2200 || got.Account != "bob" {
		t.Errorf("spec with remote's flags = port %d, account %q; want the flags to win, folded to bob", got.Port, got.Account)
	}
	overrides = config.Overrides{}

	// The record's rootfs is the path the recorded IMAGE was fetched to. A
	// client on another version must fetch its own rather than build the old
	// image under the new name.
	older := *recorded
	olderMachine := *recorded.Machine
	olderMachine.Image = "ghcr.io/example/workspace:older"
	older.Machine = &olderMachine
	if got := mustSpec(t, &machineOptions{backend: "wsl"}, "dev", &older).Rootfs; got != "" {
		t.Errorf("rootfs = %q for a record of another image, want it fetched afresh", got)
	}

	// A cache path that has been pruned names nothing, and rebuild is the way
	// back: the spec leaves it empty so EnsureRootfs fetches again.
	pruned := *recorded
	prunedMachine := *recorded.Machine
	prunedMachine.Rootfs = filepath.Join(t.TempDir(), "gone.tar")
	pruned.Machine = &prunedMachine
	if got := mustSpec(t, &machineOptions{backend: "wsl"}, "dev", &pruned).Rootfs; got != "" {
		t.Errorf("rootfs = %q for a record naming a missing file, want it fetched afresh", got)
	}

	// Create passes no record, so the defaults, not the record, are what it
	// compares the machine against.
	got := mustSpec(t, &machineOptions{backend: "wsl", cpus: 1}, "dev", nil)
	if got.CPUs != 1 || got.Rootfs != "" || got.Port != config.DefaultSSHPort || got.Account != config.DefaultUser() {
		t.Errorf("spec with no record = %+v, want the flags and the defaults alone", got)
	}
}

// `machine create` with no --rootfs fetches the published image, and running
// it again has to find the machine current rather than refuse it as built from
// different settings, which is what hashing the cache path did. `status` and
// `rebuild` must give the same answer.
func TestCreatingTwiceWithoutARootfsChangesNothing(t *testing.T) {
	saved, savedEnsure := overrides, ensureRootfs
	t.Cleanup(func() { overrides, ensureRootfs = saved, savedEnsure })
	overrides = config.Overrides{}

	// Where EnsureRootfs keeps what it fetched, so IsFetched recognises it.
	t.Setenv("LOCALAPPDATA", t.TempDir())
	ensureRootfs = func(context.Context, string, io.Writer) (string, error) {
		path := filepath.Join(os.Getenv("LOCALAPPDATA"), "remote-docker", "rootfs", "sha256-0123.tar.gz")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return "", err
		}
		return path, os.WriteFile(path, nil, 0o600)
	}

	opts := &machineOptions{backend: "wsl"}
	built, err := fetchRootfs(context.Background(), mustSpec(t, opts, "dev", nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	// What the backend writes into the machine and the workspace records.
	observed := machine.Observed{State: machine.Running, Generation: built.Generation()}
	record := machineRecord(built)

	if got := machine.Plan(mustSpec(t, opts, "dev", nil), observed); got != machine.Nothing {
		t.Errorf("creating it again plans %v, want Nothing", got)
	}
	var status strings.Builder
	reportGeneration(&status, record, observed)
	if !strings.Contains(status.String(), "current") {
		t.Errorf("status says %q about the machine create just found current", status.String())
	}

	recorded := &config.Workspace{Port: config.DefaultSSHPort, User: config.DefaultUser(), Machine: record}
	rebuilt := mustSpec(t, opts, "dev", recorded)
	if rebuilt.Rootfs != built.Rootfs || rebuilt.Generation() != built.Generation() {
		t.Errorf("rebuild = rootfs %q generation %s, want the fetched %q and %s",
			rebuilt.Rootfs, rebuilt.Generation(), built.Rootfs, built.Generation())
	}

	// A record from before the fix hashed the cache path. Create refuses that
	// machine naming rebuild, and the rebuild is what repairs it.
	old := *record
	stale := built
	stale.Fetched = false
	old.Generation = stale.Generation()
	if got := machine.Plan(mustSpec(t, opts, "dev", nil), machine.Observed{State: machine.Running, Generation: old.Generation}); got != machine.Recreate {
		t.Errorf("a machine recorded under the old generation plans %v, want Recreate", got)
	}
	recorded.Machine = &old
	if got := mustSpec(t, opts, "dev", recorded).Generation(); got != built.Generation() {
		t.Errorf("rebuilding an old record hashes %s, want %s so create agrees afterwards", got, built.Generation())
	}

	// A setting that did change is still a mismatch.
	overrides = config.Overrides{Port: 2299}
	if got := machine.Plan(mustSpec(t, opts, "dev", nil), observed); got != machine.Recreate {
		t.Errorf("a changed port plans %v, want Recreate", got)
	}
	overrides = config.Overrides{}
	named := &machineOptions{backend: "wsl", rootfs: filepath.Join(t.TempDir(), "mine.tar.gz")}
	if got := machine.Plan(mustSpec(t, named, "dev", nil), observed); got != machine.Recreate {
		t.Errorf("a different --rootfs plans %v, want Recreate", got)
	}
}

func mustSpec(t *testing.T, o *machineOptions, name string, recorded *config.Workspace) machine.Spec {
	t.Helper()
	spec, err := o.spec(name, recorded)
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

func TestMachineSpecRefusesUnderivableUser(t *testing.T) {
	overrides = config.Overrides{User: "123"}
	defer func() { overrides = config.Overrides{} }()
	if _, err := (&machineOptions{backend: "wsl"}).spec("dev", nil); err == nil || !strings.Contains(err.Error(), `"123"`) {
		t.Errorf("spec with --user 123 = %v, want an error naming it", err)
	}
}
