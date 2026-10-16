package main

import (
	"context"
	"strings"
	"testing"

	"github.com/lhns/remote-docker/client/internal/config"
	"github.com/lhns/remote-docker/machine"
)

// notYoursBackend is the service refusing a machine that is not the caller's.
type notYoursBackend struct{ fakeBackend }

func (notYoursBackend) Inspect(context.Context, string) (machine.Observed, error) {
	return machine.Observed{}, &machine.BrokerError{Message: "the workspace name is taken by another user of this computer"}
}

// A create that failed after the service built its machine and before the
// entry was written leaves a machine with no entry. `rm` of that name is how
// its owner removes it, and nothing else is touched.
func TestRmOfANameWithNoEntryDestroysOnlyTheCallersServiceMachine(t *testing.T) {
	for _, tc := range []struct {
		name      string
		installed bool
		state     machine.State
		notYours  bool
		args      []string
		destroyed bool
	}{
		{"the caller's machine", true, machine.Running, false, nil, true},
		{"a stopped one", true, machine.Stopped, false, nil, true},
		{"nothing of that name", true, machine.Absent, false, nil, false},
		{"another user's, or one the service does not manage", true, machine.Running, true, nil, false},
		{"no service", false, machine.Running, false, nil, false},
		{"--keep-machine", true, machine.Running, false, []string{"--keep-machine"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withConfig(t, &config.File{Workspaces: map[string]config.Workspace{
				"other": {Host: "127.0.0.1", Port: 1, Machine: &config.Machine{Backend: "hyperv", Name: "other", Broker: true}},
			}})
			withBrokerProbe(t, tc.installed, nil)
			log := &events{}
			var backend machine.Backend = &fakeBackend{log: log, state: tc.state}
			if tc.notYours {
				backend = &notYoursBackend{fakeBackend{log: log, state: tc.state}}
			}
			var asked []string
			savedFind := findBackend
			findBackend = func(key string) (machine.Backend, error) { asked = append(asked, key); return backend, nil }
			t.Cleanup(func() { findBackend = savedFind })

			err := run(t, append([]string{"remote", "rm", "dev", "-f"}, tc.args...)...)
			destroyed := strings.Contains(log.String(), "destroy dev")
			switch {
			case destroyed != tc.destroyed:
				t.Fatalf("destroyed = %v, want %v (err %v, events %s)", destroyed, tc.destroyed, err, log)
			case tc.destroyed && err != nil:
				t.Fatalf("err = %v", err)
			case !tc.destroyed && (err == nil || !strings.Contains(err.Error(), `"dev"`)):
				t.Fatalf("err = %v, want no workspace named dev", err)
			}
			for _, k := range asked {
				if k != "hyperv-broker" {
					t.Errorf("asked backend %q", k)
				}
			}
			file, err := config.Load("")
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := file.Workspaces["other"]; !ok || len(file.Workspaces) != 1 {
				t.Errorf("entries now %+v", file.Workspaces)
			}
		})
	}
}
