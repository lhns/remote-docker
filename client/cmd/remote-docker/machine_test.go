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
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
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

// The record pins the host key the machine was last BUILT with: a rebuild, or
// a create after `rm`, replaces it, and a create that builds nothing keeps it.
func TestMachineEntryPinsTheBuiltHostKey(t *testing.T) {
	spec := machine.Spec{Name: "dev", Backend: "wsl", Port: 2222, Account: "alice"}
	const old, rebuilt = "ssh-ed25519 AAAAold", "ssh-ed25519 AAAAnew"
	recorded := config.Workspace{Host: "127.0.0.1", Port: 2222, User: "alice",
		Machine: &config.Machine{Backend: "wsl", Name: "dev", HostKey: old}}

	for _, tc := range []struct {
		name    string
		prev    config.Workspace
		hostKey string
		wantPin string
		because string
	}{
		{"rebuild", recorded, rebuilt, rebuilt, "the destroyed machine's key would refuse the new one"},
		{"create, nothing built", recorded, "", old, "the machine still serves with the key it was built with"},
		{"create after rm", config.Workspace{}, rebuilt, rebuilt, "rm took the old record with it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := machineEntry(tc.prev, spec, tc.hostKey)
			if got.Machine.HostKey != tc.wantPin {
				t.Errorf("pinned %q, want %q: %s", got.Machine.HostKey, tc.wantPin, tc.because)
			}
			if got.Host != machinePlaceholderHost || got.Machine.Generation != spec.Generation() {
				t.Errorf("entry = %+v, machine %+v", got, *got.Machine)
			}
		})
	}
	if recorded.Machine.HostKey != old {
		t.Error("machineEntry changed the record it was handed")
	}
}

// `machine create` without --rootfs, run again, and `machine rebuild` must hash
// the fetched rootfs as the first create did, or create refuses the machine it
// built. The record holds only the path, so rebuild relies on IsFetched, which
// is also what repairs a machine recorded before Spec.Fetched.
func TestAFetchedRootfsHashesTheSameOnEveryPath(t *testing.T) {
	saved := overrides
	t.Cleanup(func() { overrides = saved })
	overrides = config.Overrides{}

	// Where EnsureRootfs keeps what it fetched, so IsFetched recognises it.
	t.Setenv("LOCALAPPDATA", t.TempDir())
	fetched := filepath.Join(os.Getenv("LOCALAPPDATA"), "remote-docker", "rootfs", "sha256-0123.tar.gz")
	if err := os.MkdirAll(filepath.Dir(fetched), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fetched, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	opts := &machineOptions{backend: "wsl"}
	// What createMachine builds once EnsureRootfs has filled in the path.
	built := mustSpec(t, opts, "dev", nil)
	built.Rootfs, built.Fetched = fetched, true

	observed := machine.Observed{State: machine.Running, Generation: built.Generation()}
	if got := machine.Plan(mustSpec(t, opts, "dev", nil), observed); got != machine.Nothing {
		t.Errorf("creating it again plans %v, want Nothing", got)
	}

	recorded := &config.Workspace{Port: config.DefaultSSHPort, User: config.DefaultUser(), Machine: machineRecord(built)}
	if got := mustSpec(t, opts, "dev", recorded); got.Rootfs != fetched || got.Generation() != built.Generation() {
		t.Errorf("rebuild = rootfs %q generation %s, want %q and %s", got.Rootfs, got.Generation(), fetched, built.Generation())
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

func TestReportAgent(t *testing.T) {
	wsl := &config.Machine{Backend: "wsl", Name: "dev"}
	hyperv := &config.Machine{Backend: "hyperv", Name: "dev"}
	up, down := listeningAgent(t), closedPort(t)
	at := func(port int) string { return "127.0.0.1:" + strconv.Itoa(port) }

	for _, tc := range []struct {
		name    string
		m       *config.Machine
		backend *fakeBackend
		port    int
		ok      bool
		want    []string
		reject  []string
	}{
		{
			name:    "answering",
			m:       wsl,
			backend: &fakeBackend{addr: "127.0.0.1"},
			port:    up,
			ok:      true,
			want:    []string{"answering on " + at(up)},
		},
		{
			// What was dialled and the remedy, never a guess at why.
			name:    "refused, wsl",
			m:       wsl,
			backend: &fakeBackend{addr: "127.0.0.1"},
			port:    down,
			want:    []string{"not answering on " + at(down), "  fix: ", machine.WSLAgentLog, "machine rebuild dev"},
		},
		{
			// The log path is WSL's.
			name:    "refused, hyperv",
			m:       hyperv,
			backend: &fakeBackend{addr: "127.0.0.1"},
			port:    down,
			want:    []string{"not answering on " + at(down), "machine rebuild dev"},
			reject:  []string{machine.WSLAgentLog},
		},
		{
			name:    "no address",
			m:       wsl,
			backend: &fakeBackend{},
			port:    up,
			want:    []string{"not checked", "no address"},
		},
		{
			name:    "address unreadable",
			m:       wsl,
			backend: &fakeBackend{addrErr: errors.New("wsl -d rd-dev ip: exit status 1\nmore detail")},
			port:    up,
			want:    []string{"not checked", "exit status 1"},
			reject:  []string{"more detail"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			if got := reportAgent(context.Background(), &out, tc.backend, tc.m, tc.port); got != tc.ok {
				t.Errorf("reportAgent = %v, want %v", got, tc.ok)
			}
			text := out.String()
			for _, w := range tc.want {
				if !strings.Contains(text, w) {
					t.Errorf("output does not contain %q:\n%s", w, text)
				}
			}
			for _, r := range tc.reject {
				if strings.Contains(text, r) {
					t.Errorf("output contains %q:\n%s", r, text)
				}
			}
			// One row, and at most one fix line under it.
			lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
			if len(lines) > 2 || (len(lines) == 2 && !strings.HasPrefix(lines[1], "  fix: ")) {
				t.Errorf("want one row and at most one fix line, got:\n%s", text)
			}
		})
	}
}

// The hint create prints must reach the machine just made, which is not the
// default when the computer already has one.
func TestCreateHintReachesTheNewMachine(t *testing.T) {
	ours := map[string]string{"dev": "dev", "old": "old"}

	for _, tc := range []struct {
		name    string
		env     map[string]string
		current string
		def     string
		want    bool
	}{
		{name: "the only workspace, so the default", def: "dev", want: true},
		{name: "another workspace is the default", def: "old", want: false},
		{name: "its context is selected", current: "dev", def: "old", want: true},
		{name: "another workspace's context is selected", current: "old", def: "dev", want: false},
		{name: "a context that is not ours", current: "desktop", def: "dev", want: false},
		{name: "DOCKER_HOST elsewhere", env: map[string]string{"DOCKER_HOST": "tcp://box:2375"}, def: "dev", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := reachedByDocker("dev", tc.def, fakeLookups(tc.env, tc.current, ours)); got != tc.want {
				t.Errorf("reachedByDocker = %v, want %v", got, tc.want)
			}
		})
	}

	if hint := tryHint("dev", true); strings.Contains(hint, " use ") {
		t.Errorf("hint for the workspace docker reaches = %q, want the run alone", hint)
	}
	if hint := tryHint("dev", false); !strings.Contains(hint, "remote use dev") || !strings.Contains(hint, "alpine ls /w") {
		t.Errorf("hint for a workspace docker does not reach = %q, want `remote use dev` then the run", hint)
	}
}

// A machine is acted on through the backend that built it. `machine rebuild`
// of a hyperv machine once ran `wsl --import` with its Flatcar disk, because
// --backend defaulted to wsl and nothing read the record.
func TestMachineCommandsUseTheRecordedBackend(t *testing.T) {
	saved := overrides
	t.Cleanup(func() { overrides = saved })
	overrides = config.Overrides{}

	hyperv := map[string]config.Workspace{
		"hvtest": {Host: "127.0.0.1", Port: 22, User: "alice", Machine: &config.Machine{Backend: "hyperv", Name: "hvtest"}},
	}
	for _, tc := range []struct {
		name    string
		records map[string]config.Workspace
		args    []string
		want    string // the backend asked for, when nothing is refused
		refusal []string
	}{
		{name: "rebuild without --backend", records: hyperv,
			args: []string{"rebuild", "hvtest", "-f"}, want: "hyperv"},
		{name: "create without --backend", records: hyperv,
			args: []string{"create", "hvtest"}, want: "hyperv"},
		{name: "create with no record", records: map[string]config.Workspace{},
			args: []string{"create", "hvtest"}, want: "wsl"},
		{name: "create naming the same backend", records: hyperv,
			args: []string{"create", "hvtest", "--backend", "hyperv"}, want: "hyperv"},
		// Two hypervisors: the old machine would be left running with
		// nothing naming it.
		{name: "rebuild onto another backend", records: hyperv,
			args:    []string{"rebuild", "hvtest", "-f", "--backend", "wsl"},
			refusal: []string{`"hvtest" is a hyperv machine`, "wsl", "  fix: ", "remote rm hvtest"}},
		{name: "create onto another backend", records: hyperv,
			args:    []string{"create", "hvtest", "--backend", "wsl"},
			refusal: []string{`"hvtest" is a hyperv machine`, "machine create hvtest --backend wsl"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withConfig(t, &config.File{Workspaces: tc.records})
			asked := ""
			stop := errors.New("asked")
			savedFind := findBackend
			findBackend = func(name string) (machine.Backend, error) { asked = name; return nil, stop }
			t.Cleanup(func() { findBackend = savedFind })

			err := run(t, append([]string{"remote", "machine"}, tc.args...)...)
			if tc.refusal == nil {
				if !errors.Is(err, stop) {
					t.Fatalf("err = %v, want the backend to be asked for", err)
				}
				if asked != tc.want {
					t.Errorf("asked for the %q backend, want %q", asked, tc.want)
				}
				return
			}
			if err == nil || errors.Is(err, stop) {
				t.Fatalf("err = %v, want a refusal before any backend is asked (asked %q)", err, asked)
			}
			for _, w := range tc.refusal {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("the refusal does not contain %q:\n%v", w, err)
				}
			}
		})
	}
}
