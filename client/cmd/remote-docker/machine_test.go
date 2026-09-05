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
	"strings"
	"testing"

	"github.com/lhns/remote-docker/client/internal/config"
	"github.com/lhns/remote-docker/client/internal/machine"
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
	image := machine.DefaultImage(version)
	recorded := &config.Machine{Backend: "wsl", Name: "dev", Image: image, Rootfs: "/cache/rootfs.tar", CPUs: 4, MemoryMB: 8192}

	unset := (&machineOptions{backend: "wsl"}).spec("dev", recorded)
	if unset.CPUs != 4 || unset.MemoryMB != 8192 || unset.Rootfs != "/cache/rootfs.tar" {
		t.Errorf("spec with no flags = cpus %d, memory %d, rootfs %q; want the recorded 4, 8192, /cache/rootfs.tar",
			unset.CPUs, unset.MemoryMB, unset.Rootfs)
	}

	set := (&machineOptions{backend: "wsl", cpus: 2, memoryMB: 1024, rootfs: "/mine.tar"}).spec("dev", recorded)
	if set.CPUs != 2 || set.MemoryMB != 1024 || set.Rootfs != "/mine.tar" {
		t.Errorf("spec with flags = cpus %d, memory %d, rootfs %q; want the flags to win",
			set.CPUs, set.MemoryMB, set.Rootfs)
	}

	// The record's rootfs is the path the recorded IMAGE was fetched to. A
	// client on another version must fetch its own rather than build the old
	// image under the new name.
	older := *recorded
	older.Image = "ghcr.io/example/workspace:older"
	if got := (&machineOptions{backend: "wsl"}).spec("dev", &older).Rootfs; got != "" {
		t.Errorf("rootfs = %q for a record of another image, want it fetched afresh", got)
	}

	if got := (&machineOptions{backend: "wsl", cpus: 1}).spec("dev", nil); got.CPUs != 1 || got.Rootfs != "" {
		t.Errorf("spec with no record = %+v, want the flags alone", got)
	}
}

// The name selects the workspace and is never the config path: a name passed
// as the path resolves the DEFAULT workspace, so `machine stop other` would
// shut down the default's session.
func TestSessionEndpointForNamesTheWorkspaceAsked(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv(config.EnvWorkspace, "")
	t.Setenv(config.EnvEndpoint, "")

	file := config.File{
		Default: "main",
		Workspaces: map[string]config.Workspace{
			"main":  {Host: "main.example", Endpoint: "/tmp/main.sock"},
			"other": {Host: "127.0.0.1", Endpoint: "/tmp/other.sock", Machine: &config.Machine{Backend: "wsl", Name: "rd-other"}},
		},
	}
	if err := config.Save(file, ""); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := sessionEndpointFor("other")
	if err != nil {
		t.Fatalf("sessionEndpointFor: %v", err)
	}
	if got != "/tmp/other.sock" {
		t.Errorf("endpoint for %q = %q, want /tmp/other.sock and not the default workspace's", "other", got)
	}
}
