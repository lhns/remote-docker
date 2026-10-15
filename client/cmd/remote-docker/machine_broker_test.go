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

// brokerBackend records the key it was found by and the spec it built from.
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

	var asked []string
	savedFind := findBackend
	findBackend = func(key string) (machine.Backend, error) { asked = append(asked, key); return be, nil }
	t.Cleanup(func() { findBackend = savedFind })
	savedPrepare := prepareRootfs
	prepareRootfs = func(context.Context, string, string, string, io.Writer) (string, error) {
		t.Error("the client downloaded a disk the service fetches itself")
		return "", errors.New("unexpected")
	}
	t.Cleanup(func() { prepareRootfs = savedPrepare })

	// Fails after the record is written, at the wait for the agent.
	_ = run(t, "remote", "machine", "create", "dev", "--backend", "hyperv")

	if !slices.Contains(asked, "hyperv-broker") || slices.Contains(asked, "hyperv") {
		t.Errorf("backends asked for: %v", asked)
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
	var asked []string
	savedFind := findBackend
	findBackend = func(key string) (machine.Backend, error) {
		asked = append(asked, key)
		return &fakeBackend{log: log}, nil
	}
	t.Cleanup(func() { findBackend = savedFind })

	_ = run(t, "remote", "machine", "stop", "dev", "-f")
	_ = run(t, "remote", "machine", "status", "dev")
	_ = run(t, "remote", "rm", "dev", "-f")
	_ = run(t, "remote", "rm", "old", "-f")

	want := []string{"hyperv-broker", "hyperv-broker", "hyperv-broker", "hyperv"}
	if !slices.Equal(asked, want) {
		t.Errorf("backends asked for: %v, want %v", asked, want)
	}
	if got := log.String(); !strings.Contains(got, "destroy dev") || !strings.Contains(got, "destroy old") {
		t.Errorf("events: %s", got)
	}
}
