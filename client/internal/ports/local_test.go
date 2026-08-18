package ports

import (
	"slices"
	"testing"
)

// The two numbers are allowed to differ: the workspace daemon chose 32768 and
// the user typed 8080, so the listener is theirs and the dial is the daemon's.
func TestTheLocalPortIsTheOneTheUserAskedFor(t *testing.T) {
	docker := &fakeDocker{containers: []Container{
		{ID: "a", Name: "web", Ports: []Published{tcp(32768, 80)}},
	}}
	fwd := newForwarder()
	m := &Manager{
		Docker:    docker,
		Forwarder: fwd,
		LocalPort: func(_ Container, p Published) int {
			if p.PrivatePort == 80 {
				return 8080
			}
			return 0
		},
	}

	if err := m.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !slices.Equal(fwd.opened, []string{"127.0.0.1:8080"}) {
		t.Errorf("opened %v, want the port the user asked for", fwd.opened)
	}
	if got := fwd.remotes["127.0.0.1:8080"]; got != "127.0.0.1:32768" {
		t.Errorf("it dials %q, want the port the daemon published", got)
	}
}

// A container with no answer, which is one created before this or by something
// that is not us, is forwarded exactly as it always was.
func TestWithoutAnAnswerTheLocalPortIsThePublishedOne(t *testing.T) {
	docker := &fakeDocker{containers: []Container{
		{ID: "a", Name: "web", Ports: []Published{tcp(8080, 80)}},
	}}
	fwd := newForwarder()
	m := &Manager{
		Docker:    docker,
		Forwarder: fwd,
		LocalPort: func(Container, Published) int { return 0 },
	}

	if err := m.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !slices.Equal(fwd.opened, []string{"127.0.0.1:8080"}) {
		t.Errorf("opened %v, want the published port", fwd.opened)
	}
}

// Forwarding is what refuses a second container asking for a port this session
// already opened, so it has to answer about the LOCAL number rather than the
// published one they are keyed on.
func TestForwardingAnswersAboutTheLocalPort(t *testing.T) {
	docker := &fakeDocker{containers: []Container{
		{ID: "a", Name: "web", Ports: []Published{tcp(32768, 80)}},
	}}
	m := &Manager{
		Docker:    docker,
		Forwarder: newForwarder(),
		LocalPort: func(Container, Published) int { return 8080 },
	}
	if err := m.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if !m.Forwarding(8080) {
		t.Error("the port the listener is on reads as free")
	}
	if m.Forwarding(32768) {
		t.Error("the published port reads as taken on this machine, where nothing is listening on it")
	}
}
