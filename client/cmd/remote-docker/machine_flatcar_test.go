package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/lhns/remote-docker/client/internal/config"
)

// The rootfs a machine is built from is chosen per backend, and Hyper-V's proxy
// reaches that choice. Stopped at the fetch, which is the point under test.
func TestCreateFetchesTheDiskOfTheMachinesBackend(t *testing.T) {
	stop := errors.New("stop at the fetch")
	for _, tc := range []struct {
		backend, proxy string
	}{
		{"hyperv", "http://proxy.example:3128"},
		{"wsl", ""},
	} {
		t.Run(tc.backend, func(t *testing.T) {
			withConfig(t, nil)
			withBackend(t, &fakeBackend{log: &events{}})

			var gotBackend, gotProxy string
			saved := prepareRootfs
			prepareRootfs = func(_ context.Context, backend, _, proxy string, _ io.Writer) (string, error) {
				gotBackend, gotProxy = backend, proxy
				return "", stop
			}
			t.Cleanup(func() { prepareRootfs = saved })

			args := []string{"remote", "machine", "create", "dev", "--backend", tc.backend}
			if tc.proxy != "" {
				args = append(args, "--proxy", tc.proxy)
			}
			if err := run(t, args...); !errors.Is(err, stop) {
				t.Fatalf("err = %v, want the fetch to be reached", err)
			}
			if gotBackend != tc.backend || gotProxy != tc.proxy {
				t.Errorf("fetched for backend %q proxy %q, want %q %q", gotBackend, gotProxy, tc.backend, tc.proxy)
			}
		})
	}
}

func TestProxyIsHyperVOnlyAndRecorded(t *testing.T) {
	_, err := (&machineOptions{backend: "wsl", proxy: "http://proxy.example:3128"}).spec("dev", nil)
	if err == nil || !strings.Contains(err.Error(), "hyperv") {
		t.Errorf("wsl with --proxy: err = %v, want a refusal naming hyperv", err)
	}
	if _, err := (&machineOptions{backend: "hyperv", proxy: "proxy.example"}).spec("dev", nil); err == nil {
		t.Error("a proxy that is not a URL was accepted")
	}

	// A rebuild without --proxy keeps the one the machine was built with.
	built := mustSpec(t, &machineOptions{backend: "hyperv", proxy: "http://proxy.example:3128"}, "dev", nil)
	recorded := &config.Workspace{Port: config.DefaultSSHPort, User: config.DefaultUser(), Machine: machineRecord(built)}
	if got := mustSpec(t, &machineOptions{}, "dev", recorded); got.Proxy != built.Proxy || got.Generation() != built.Generation() {
		t.Errorf("rebuild proxy = %q generation %s, want %q and %s", got.Proxy, got.Generation(), built.Proxy, built.Generation())
	}
}
