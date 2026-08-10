package ports

import (
	"context"
	"fmt"
	"net"
	"slices"
	"sync"
	"testing"
)

// fakeForward records whether it was closed.
type fakeForward struct {
	local  net.Addr
	closed bool
}

func (f *fakeForward) Close() error        { f.closed = true; return nil }
func (f *fakeForward) LocalAddr() net.Addr { return f.local }

type addr string

func (a addr) Network() string { return "tcp" }
func (a addr) String() string  { return string(a) }

// fakeForwarder hands out fakeForwards and can be told to refuse specific
// local addresses, standing in for a port already in use.
type fakeForwarder struct {
	mu       sync.Mutex
	opened   []string
	forwards map[string]*fakeForward
	refuse   map[string]bool
}

func newForwarder() *fakeForwarder {
	return &fakeForwarder{forwards: map[string]*fakeForward{}, refuse: map[string]bool{}}
}

func (f *fakeForwarder) Forward(local, _ string) (Forward, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refuse[local] {
		return nil, fmt.Errorf("bind %s: address already in use", local)
	}
	f.opened = append(f.opened, local)
	fwd := &fakeForward{local: addr(local)}
	f.forwards[local] = fwd
	return fwd, nil
}

type fakeDocker struct{ containers []Container }

func (f *fakeDocker) ListContainers(context.Context) ([]Container, error) {
	return f.containers, nil
}

func tcp(public, private int) Published {
	return Published{PublicPort: public, PrivatePort: private, Type: "tcp"}
}

func TestReconcileOpensForwardsForPublishedPorts(t *testing.T) {
	docker := &fakeDocker{containers: []Container{
		{ID: "a", Name: "web", Ports: []Published{tcp(8080, 80)}},
	}}
	fwd := newForwarder()
	m := &Manager{Docker: docker, Forwarder: fwd}

	if err := m.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := m.Active(); !slices.Equal(got, []int{8080}) {
		t.Errorf("Active() = %v, want [8080]", got)
	}
	if len(fwd.opened) != 1 || fwd.opened[0] != "127.0.0.1:8080" {
		t.Errorf("opened %v, want [127.0.0.1:8080]", fwd.opened)
	}
}

// The published port must be reachable at the address the user asked for.
// Remapping it would produce a working listener nobody can find.
func TestReconcileUsesTheRequestedPort(t *testing.T) {
	docker := &fakeDocker{containers: []Container{
		{ID: "a", Name: "web", Ports: []Published{tcp(3000, 3000), tcp(5432, 5432)}},
	}}
	fwd := newForwarder()
	m := &Manager{Docker: docker, Forwarder: fwd}

	if err := m.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	for _, want := range []string{"127.0.0.1:3000", "127.0.0.1:5432"} {
		if !slices.Contains(fwd.opened, want) {
			t.Errorf("opened %v, missing %s", fwd.opened, want)
		}
	}
}

func TestReconcileClosesForwardsWhenContainersStop(t *testing.T) {
	docker := &fakeDocker{containers: []Container{
		{ID: "a", Name: "web", Ports: []Published{tcp(8080, 80)}},
	}}
	fwd := newForwarder()
	m := &Manager{Docker: docker, Forwarder: fwd}

	if err := m.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	docker.containers = nil
	if err := m.Reconcile(t.Context()); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}

	if got := m.Active(); len(got) != 0 {
		t.Errorf("Active() = %v, want empty", got)
	}
	if !fwd.forwards["127.0.0.1:8080"].closed {
		t.Error("the forward was not closed when its container stopped")
	}
}

// Reconciliation is what makes a dropped event stream survivable: recomputing
// from current state cannot leak a forward or miss a container, where applying
// events incrementally does both across a gap.
func TestReconcileIsIdempotent(t *testing.T) {
	docker := &fakeDocker{containers: []Container{
		{ID: "a", Name: "web", Ports: []Published{tcp(8080, 80)}},
	}}
	fwd := newForwarder()
	m := &Manager{Docker: docker, Forwarder: fwd}

	for range 5 {
		if err := m.Reconcile(t.Context()); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}
	if len(fwd.opened) != 1 {
		t.Errorf("opened %v; repeated reconciliation should not reopen a live forward", fwd.opened)
	}
	if got := m.Active(); !slices.Equal(got, []int{8080}) {
		t.Errorf("Active() = %v, want [8080]", got)
	}
}

// A container that survives but drops a port must lose that forward alone.
func TestReconcileClosesOnlyWithdrawnPorts(t *testing.T) {
	docker := &fakeDocker{containers: []Container{
		{ID: "a", Name: "web", Ports: []Published{tcp(8080, 80), tcp(9090, 90)}},
	}}
	fwd := newForwarder()
	m := &Manager{Docker: docker, Forwarder: fwd}

	if err := m.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	docker.containers[0].Ports = []Published{tcp(8080, 80)}
	if err := m.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}

	if got := m.Active(); !slices.Equal(got, []int{8080}) {
		t.Errorf("Active() = %v, want [8080]", got)
	}
	if fwd.forwards["127.0.0.1:8080"].closed {
		t.Error("a still-published port was closed")
	}
	if !fwd.forwards["127.0.0.1:9090"].closed {
		t.Error("a withdrawn port was not closed")
	}
}

// With a shared daemon the event stream carries other users' containers.
// Forwarding those would open listeners on this machine because somebody else
// ran docker compose up.
func TestReconcileIgnoresContainersWeDoNotOwn(t *testing.T) {
	docker := &fakeDocker{containers: []Container{
		{ID: "mine", Name: "web", Ports: []Published{tcp(8080, 80)},
			Labels: map[string]string{"owner": "me"}},
		{ID: "theirs", Name: "their-web", Ports: []Published{tcp(9999, 80)},
			Labels: map[string]string{"owner": "someone-else"}},
	}}
	fwd := newForwarder()
	m := &Manager{
		Docker:    docker,
		Forwarder: fwd,
		Owned:     func(c Container) bool { return c.Labels["owner"] == "me" },
	}

	if err := m.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := m.Active(); !slices.Equal(got, []int{8080}) {
		t.Errorf("Active() = %v, want [8080] only", got)
	}
}

// One unavailable port must not stop every other container's ports being
// forwarded.
func TestReconcileSurvivesAPortConflict(t *testing.T) {
	docker := &fakeDocker{containers: []Container{
		{ID: "a", Name: "web", Ports: []Published{tcp(8080, 80)}},
		{ID: "b", Name: "api", Ports: []Published{tcp(9090, 90)}},
	}}
	fwd := newForwarder()
	fwd.refuse["127.0.0.1:8080"] = true
	m := &Manager{Docker: docker, Forwarder: fwd}

	if err := m.Reconcile(t.Context()); err != nil {
		t.Fatalf("a port conflict aborted the whole reconciliation: %v", err)
	}
	if got := m.Active(); !slices.Equal(got, []int{9090}) {
		t.Errorf("Active() = %v, want [9090]", got)
	}
	// Never silently remapped onto a different local port.
	for _, opened := range fwd.opened {
		if opened != "127.0.0.1:9090" {
			t.Errorf("unexpected forward %s; a conflict must not be remapped", opened)
		}
	}
}

func TestPublishedTCP(t *testing.T) {
	got := publishedTCP(Container{Ports: []Published{
		{PublicPort: 8080, PrivatePort: 80, Type: "tcp"},
		{PublicPort: 0, PrivatePort: 443, Type: "tcp"},   // exposed, not published
		{PublicPort: 5353, PrivatePort: 53, Type: "udp"}, // SSH carries TCP only
		{PublicPort: 8080, PrivatePort: 80, Type: "tcp"}, // IPv6 duplicate
		{PublicPort: 9090, PrivatePort: 90, Type: "tcp"},
	}})

	if len(got) != 2 {
		t.Fatalf("publishedTCP returned %d ports, want 2: %+v", len(got), got)
	}
	if got[0].PublicPort != 8080 || got[1].PublicPort != 9090 {
		t.Errorf("got %+v, want 8080 then 9090", got)
	}
}

func TestCloseTearsDownEverything(t *testing.T) {
	docker := &fakeDocker{containers: []Container{
		{ID: "a", Name: "web", Ports: []Published{tcp(8080, 80)}},
	}}
	fwd := newForwarder()
	m := &Manager{Docker: docker, Forwarder: fwd}

	if err := m.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !fwd.forwards["127.0.0.1:8080"].closed {
		t.Error("Close left a forward open")
	}
	if got := m.Active(); len(got) != 0 {
		t.Errorf("Active() = %v after Close, want empty", got)
	}
}
