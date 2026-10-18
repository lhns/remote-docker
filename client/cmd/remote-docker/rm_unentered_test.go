package main

import (
	"context"
	"strings"
	"testing"

	"github.com/lhns/remote-docker/client/internal/config"
	"github.com/lhns/remote-docker/machine"
)

// refusingBackend is the service refusing to answer, with the given code.
type refusingBackend struct {
	fakeBackend
	code string
}

func (b refusingBackend) Inspect(context.Context, string) (machine.Observed, error) {
	return machine.Observed{}, &machine.BrokerError{Code: b.code, Message: "refused: " + b.code}
}

// A create that failed after the service built its machine and before the
// entry was written leaves a machine with no entry. `rm` of that name is how
// its owner removes it, and nothing else is touched.
func TestRmOfANameWithNoEntryDestroysOnlyTheCallersServiceMachine(t *testing.T) {
	for _, tc := range []struct {
		name      string
		installed bool
		state     machine.State
		refusal   string // the service's refusal code, or "" for an answer
		args      []string
		destroyed bool
		wantErr   string
	}{
		{"the caller's machine", true, machine.Running, "", nil, true, ""},
		{"a stopped one", true, machine.Stopped, "", nil, true, ""},
		{"nothing of that name", true, machine.Absent, "", nil, false, `no workspace named "dev"`},
		{"another user's, or one the service does not manage", true, machine.Running, "not-yours", nil, false, `no workspace named "dev"`},
		// The service could not answer: say why, not that the name is unknown.
		{"the service refusing the caller", true, machine.Running, "not-allowed", nil, false, "may not use the Hyper-V service"},
		{"no service", false, machine.Running, "", nil, false, `no workspace named "dev"`},
		{"--keep-machine", true, machine.Running, "", []string{"--keep-machine"}, false, `"dev"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withConfig(t, &config.File{Workspaces: map[string]config.Workspace{
				"other": {Host: "127.0.0.1", Port: 1, Machine: &config.Machine{Backend: "hyperv", Name: "other", Broker: true}},
			}})
			withBrokerProbe(t, tc.installed, nil)
			log := &events{}
			var backend machine.Backend = &fakeBackend{log: log, state: tc.state}
			if tc.refusal != "" {
				backend = &refusingBackend{fakeBackend{log: log, state: tc.state}, tc.refusal}
			}
			asked := withBackendAsked(t, backend)

			err := run(t, append([]string{"remote", "rm", "dev", "-f"}, tc.args...)...)
			destroyed := strings.Contains(log.String(), "destroy dev")
			switch {
			case destroyed != tc.destroyed:
				t.Fatalf("destroyed = %v, want %v (err %v, events %s)", destroyed, tc.destroyed, err, log)
			case tc.destroyed && err != nil:
				t.Fatalf("err = %v", err)
			case !tc.destroyed && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
			}
			for _, k := range *asked {
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
