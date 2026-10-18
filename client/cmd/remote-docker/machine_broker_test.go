package main

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/lhns/remote-docker/client/internal/config"
	"github.com/lhns/remote-docker/machine"
)

func withBrokerProbe(t *testing.T, installed bool, err error) {
	t.Helper()
	saved := brokerInstalled
	brokerInstalled = func() (bool, error) { return installed, err }
	t.Cleanup(func() { brokerInstalled = saved })
}

// withBackendAsked is withBackend, also recording the key each lookup named.
func withBackendAsked(t *testing.T, b machine.Backend) *[]string {
	t.Helper()
	var asked []string
	saved := findBackend
	findBackend = func(key string) (machine.Backend, error) { asked = append(asked, key); return b, nil }
	t.Cleanup(func() { findBackend = saved })
	return &asked
}

// A new Hyper-V machine goes through the service when it is installed; one
// that exists keeps what it was built with.
func TestDecideBroker(t *testing.T) {
	broker := &config.Workspace{Machine: &config.Machine{Backend: "hyperv", Name: "dev", Broker: true}}
	direct := &config.Workspace{Machine: &config.Machine{Backend: "hyperv", Name: "dev"}}

	for _, tc := range []struct {
		name      string
		backend   string
		installed bool
		probeErr  error
		recorded  *config.Workspace
		want      bool
	}{
		{"new, service installed", "hyperv", true, nil, nil, true},
		{"new, no service", "hyperv", false, nil, nil, false},
		{"new, the probe fails", "hyperv", false, errors.New("scm"), nil, false},
		{"recorded through the service", "hyperv", false, nil, broker, true},
		{"recorded direct, service since installed", "hyperv", true, nil, direct, false},
		{"wsl never", "wsl", true, nil, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withBrokerProbe(t, tc.installed, tc.probeErr)
			spec := machine.Spec{Name: "dev", Backend: tc.backend}
			if err := decideBroker(&spec, tc.recorded, ""); err != nil {
				t.Fatal(err)
			}
			if spec.Broker != tc.want {
				t.Errorf("Broker = %v, want %v", spec.Broker, tc.want)
			}
		})
	}

	withBrokerProbe(t, true, nil)
	spec := machine.Spec{Name: "dev", Backend: "hyperv"}
	if err := decideBroker(&spec, nil, `C:\x.vhdx`); err == nil {
		t.Error("--rootfs was accepted with the service, which would open the path as SYSTEM")
	}
}

// brokerFakeBackend records the spec it built from.
type brokerFakeBackend struct {
	fakeBackend
	built *machine.Spec
}

func (b *brokerFakeBackend) Create(_ context.Context, s machine.Spec) error {
	b.built = &s
	return nil
}

func TestCreateThroughTheServiceRecordsBrokerAndFetchesNothing(t *testing.T) {
	withConfig(t, nil)
	withBrokerProbe(t, true, nil)
	be := &brokerFakeBackend{fakeBackend: fakeBackend{log: &events{}}}
	asked := withBackendAsked(t, be)
	savedPrepare := prepareRootfs
	prepareRootfs = func(context.Context, string, string, string, io.Writer) (string, error) {
		t.Error("the client downloaded a disk the service fetches itself")
		return "", errors.New("unexpected")
	}
	t.Cleanup(func() { prepareRootfs = savedPrepare })

	// Fails after the record is written, at the wait for the agent.
	_ = run(t, "remote", "machine", "create", "dev", "--backend", "hyperv")

	if !slices.Contains(*asked, "hyperv-broker") || slices.Contains(*asked, "hyperv") {
		t.Errorf("backends asked for: %v", *asked)
	}
	if be.built == nil || be.built.Rootfs != "" || !be.built.Broker {
		t.Fatalf("built from %+v", be.built)
	}
	file, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	m := file.Workspaces["dev"].Machine
	if m == nil || !m.Broker || m.Backend != "hyperv" || m.BackendKey() != "hyperv-broker" {
		t.Errorf("recorded machine = %+v", m)
	}
}

func TestEveryOperationOnABrokerMachineGoesThroughTheBroker(t *testing.T) {
	withConfig(t, &config.File{Default: "dev", Workspaces: map[string]config.Workspace{
		"dev": {Host: "127.0.0.1", Port: 1, Machine: &config.Machine{Backend: "hyperv", Name: "dev", Broker: true}},
		"old": {Host: "127.0.0.1", Port: 1, Machine: &config.Machine{Backend: "hyperv", Name: "old"}},
	}})
	log := &events{}
	asked := withBackendAsked(t, &fakeBackend{log: log})

	_ = run(t, "remote", "machine", "stop", "dev", "-f")
	_ = run(t, "remote", "machine", "status", "dev")
	_ = run(t, "remote", "rm", "dev", "-f")
	_ = run(t, "remote", "rm", "old", "-f")

	want := []string{"hyperv-broker", "hyperv-broker", "hyperv-broker", "hyperv"}
	if !slices.Equal(*asked, want) {
		t.Errorf("backends asked for: %v, want %v", *asked, want)
	}
	if got := log.String(); !strings.Contains(got, "destroy dev") || !strings.Contains(got, "destroy old") {
		t.Errorf("events: %s", got)
	}
}

// hyperv-broker is what a record says, not something to ask for: it would be
// recorded as a backend of its own.
func TestTheBrokeredBackendCannotBeNamedOnTheCommandLine(t *testing.T) {
	_, err := (&machineOptions{backend: "hyperv-broker"}).spec("dev", nil)
	if err == nil || !strings.Contains(err.Error(), "hyperv") {
		t.Errorf("err = %v", err)
	}
}

type goneServiceBackend struct{ fakeBackend }

func (goneServiceBackend) Destroy(context.Context, string) error {
	return machine.ErrBrokerNotInstalled
}

// After the service is uninstalled its machines are gone with it, and `rm` says
// how to forget the workspace.
func TestRmOfABrokerMachineAfterUninstallPointsAtKeepMachine(t *testing.T) {
	withConfig(t, &config.File{Default: "dev", Workspaces: map[string]config.Workspace{
		"dev": {Host: "127.0.0.1", Port: 1, Machine: &config.Machine{Backend: "hyperv", Name: "dev", Broker: true}},
	}})
	withBackend(t, &goneServiceBackend{fakeBackend{log: &events{}}})

	err := run(t, "remote", "rm", "dev", "-f")
	if err == nil || !strings.Contains(err.Error(), "rm dev --keep-machine` forgets the workspace") {
		t.Errorf("err = %v", err)
	}
}

// The installer's guard before it schedules its rollback `remote rm`: any
// entry with the name, machine or not, and any machine with it, is refused.
func TestRefuseExistingWorkspace(t *testing.T) {
	withBrokerProbe(t, true, nil)
	for _, tc := range []struct {
		name    string
		entries map[string]config.Workspace
		state   machine.State
		refused string
	}{
		{"nothing there", nil, machine.Absent, ""},
		{"a plain workspace", map[string]config.Workspace{"hyperv": {Host: "build.example", Port: 22}}, machine.Absent, "already exists"},
		{"a machine workspace", map[string]config.Workspace{"hyperv": {Host: "127.0.0.1", Port: 2222,
			Machine: &config.Machine{Backend: "hyperv", Name: "hyperv", Broker: true}}}, machine.Running, "already exists"},
		{"a machine with no entry", nil, machine.Stopped, "no workspace for it"},
		{"another name is no obstacle", map[string]config.Workspace{"other": {Host: "build.example", Port: 22}}, machine.Absent, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withConfig(t, &config.File{Workspaces: tc.entries})
			asked := withBackendAsked(t, &fakeBackend{log: &events{}, state: tc.state})

			err := run(t, "remote", "machine", "service", "refuse-existing", "hyperv")
			switch {
			case tc.refused == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.refused != "" && (err == nil || !strings.Contains(err.Error(), tc.refused)):
				t.Fatalf("err = %v, want one saying %q", err, tc.refused)
			}
			if len(*asked) > 0 && !slices.Equal(*asked, []string{"hyperv-broker"}) {
				t.Errorf("asked backends %v, want the one create would use", *asked)
			}
			file, err := config.Load("")
			if err != nil {
				t.Fatal(err)
			}
			if len(file.Workspaces) != len(tc.entries) {
				t.Errorf("the check changed the config: %+v", file.Workspaces)
			}
		})
	}
}
