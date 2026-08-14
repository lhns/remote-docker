package daemons

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeDocker answers the two questions idle asks: the parent for a container's
// state, and the account's own daemon for what it is running.
type fakeDocker struct {
	host string

	// state is what the parent reports, or "" for no such container.
	state string

	// running is what the account's daemon answers `ps --quiet` with, and
	// unreachable makes it answer nothing at all, which is what a daemon that
	// is not up looks like from here.
	running     string
	unreachable bool

	// listing is what the parent answers `ps --all` with, one JSON row per
	// line, and ran records every command it was asked to Run.
	listing string
	ran     *[]string
}

func (f *fakeDocker) Line(_ context.Context, args ...string) (string, error) {
	joined := strings.Join(args, " ")
	switch {
	case f.host == "" && strings.HasPrefix(joined, "inspect"):
		if f.state == "" {
			return "", errors.New("no such container")
		}
		return f.state, nil
	case f.host == "" && strings.HasPrefix(joined, "ps"):
		return f.listing, nil
	case f.host != "" && strings.HasPrefix(joined, "ps"):
		if f.unreachable {
			return "", errors.New("cannot connect to the docker daemon")
		}
		return f.running, nil
	}
	return "", errors.New("fakeDocker: unexpected " + joined)
}

func (f *fakeDocker) Run(_ context.Context, _ string, args ...string) error {
	if f.ran != nil {
		*f.ran = append(*f.ran, strings.Join(args, " "))
	}
	return nil
}

func (f *fakeDocker) Output(_ context.Context, _ ...string) ([]byte, error) { return nil, nil }

// managerWith builds a manager whose docker command is the fake above.
func managerWith(state, running string, unreachable bool) *Manager {
	return &Manager{docker: func(host string) docker {
		return &fakeDocker{host: host, state: state, running: running, unreachable: unreachable}
	}}
}

// A daemon that is not running cannot be running anything, whatever it says
// when asked -- and it says nothing, because it is not up to answer.
//
// This is the rule that left a real workspace broken for as long as it stayed
// broken. reconcile would not replace a daemon built from stale settings unless
// it could prove nothing was running inside, and it asked the daemon. A
// crash-looping container never answers, "cannot tell" counted as busy, and the
// one daemon that most needed rebuilding was the one the rule protected. It
// logged "has containers running" about a container that was restarting every
// nineteen seconds.
func TestABrokenDaemonIsNotBusy(t *testing.T) {
	for _, state := range []string{"restarting", "exited", "created", "dead"} {
		t.Run(state, func(t *testing.T) {
			m := managerWith(state, "", true)
			if !m.idle(context.Background(), "alice", "rd-dind-alice") {
				t.Errorf("a %s daemon counts as busy, so it can never be recreated", state)
			}
		})
	}
}

// The rule the old one was written for, which must survive: a daemon that IS
// running and cannot be asked is left alone, because the cost of being wrong
// is somebody's containers.
func TestARunningDaemonThatCannotBeAskedIsBusy(t *testing.T) {
	m := managerWith("running", "", true)
	if m.idle(context.Background(), "alice", "rd-dind-alice") {
		t.Error("a running daemon that cannot be asked was treated as idle")
	}
}

func TestARunningDaemonWithContainersIsBusy(t *testing.T) {
	m := managerWith("running", "abc123\ndef456", false)
	if m.idle(context.Background(), "alice", "rd-dind-alice") {
		t.Error("a daemon with two containers running was treated as idle")
	}
}

func TestARunningDaemonWithNothingInsideIsIdle(t *testing.T) {
	m := managerWith("running", "", false)
	if !m.idle(context.Background(), "alice", "rd-dind-alice") {
		t.Error("an empty daemon was treated as busy, so a setting would never apply")
	}
}

// No container at all: nothing to protect, and Ensure is about to create one.
func TestAMissingDaemonIsIdle(t *testing.T) {
	m := managerWith("", "", true)
	if !m.idle(context.Background(), "alice", "rd-dind-alice") {
		t.Error("a daemon that does not exist was treated as busy")
	}
}

// In shared mode nothing routes to a per-account daemon, so one left running by
// a previous per-account run is stopped. Never removed: the container is
// somebody's daemon and the volume behind it is their images and containers.
func TestStopStraysStopsRunningDaemonsAndRemovesNothing(t *testing.T) {
	var ran []string
	rows := strings.Join([]string{
		`{"Names":"rd-dind-alice","Labels":"remote-docker.account=alice","State":"running"}`,
		`{"Names":"rd-dind-bob","Labels":"remote-docker.account=bob","State":"restarting"}`,
		`{"Names":"rd-dind-carol","Labels":"remote-docker.account=carol","State":"exited"}`,
	}, "\n")

	m := &Manager{Options: Options{Workspace: "ws1"}, docker: func(host string) docker {
		return &fakeDocker{host: host, listing: rows, ran: &ran}
	}}

	n, err := m.StopStrays(context.Background())
	if err != nil {
		t.Fatalf("StopStrays: %v", err)
	}
	if n != 2 {
		t.Errorf("stopped %d daemons, want the running one and the restarting one", n)
	}
	want := []string{"stop rd-dind-alice", "stop rd-dind-bob"}
	if strings.Join(ran, ",") != strings.Join(want, ",") {
		t.Errorf("ran %v, want %v", ran, want)
	}
	for _, cmd := range ran {
		if strings.HasPrefix(cmd, "rm") {
			t.Errorf("a daemon was removed rather than stopped: %q", cmd)
		}
	}
}

// A crash-looping daemon is the case this exists for: it restarts forever in a
// mode that never sends it a session, and nothing else would stop it.
func TestStopStraysStopsACrashLoopingDaemon(t *testing.T) {
	var ran []string
	m := &Manager{Options: Options{Workspace: "ws1"}, docker: func(host string) docker {
		return &fakeDocker{
			host:    host,
			listing: `{"Names":"rd-dind-alice","Labels":"remote-docker.account=alice","State":"restarting"}`,
			ran:     &ran,
		}
	}}

	if n, err := m.StopStrays(context.Background()); err != nil || n != 1 {
		t.Fatalf("StopStrays = %d, %v; want 1, nil", n, err)
	}
}

// Without a workspace id the listing is not filtered to our own daemons, and a
// parent shared with another workspace would have its daemons stopped by us.
// Refused by name rather than left to whoever calls it.
func TestStopStraysRefusesWithoutAWorkspaceID(t *testing.T) {
	var ran []string
	m := &Manager{docker: func(host string) docker {
		return &fakeDocker{
			host:    host,
			listing: `{"Names":"rd-dind-alice","Labels":"remote-docker.account=alice","State":"running"}`,
			ran:     &ran,
		}
	}}

	if _, err := m.StopStrays(context.Background()); err == nil {
		t.Error("StopStrays ran unfiltered")
	}
	if len(ran) != 0 {
		t.Errorf("it stopped something anyway: %v", ran)
	}
}
