package ephemeral

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePorts hands out ports downward and remembers what was freed.
type fakePorts struct {
	mu    sync.Mutex
	next  int
	token uint64
	held  map[string]uint64 // client -> token
	ports map[string]int
	freed []string
}

func newFakePorts() *fakePorts {
	return &fakePorts{next: 65535, held: map[string]uint64{}, ports: map[string]int{}}
}

func (f *fakePorts) ForRun(_, client string) (int, uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.held[client]; ok {
		return f.ports[client], t, nil
	}
	f.token++
	f.held[client], f.ports[client] = f.token, f.next
	f.next--
	return f.ports[client], f.token, nil
}

func (f *fakePorts) Hold(_, client string, port int) (uint64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.token++
	f.held[client], f.ports[client] = f.token, port
	return f.token, true
}

func (f *fakePorts) Free(_, client string, token uint64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.held[client] != token {
		return false
	}
	delete(f.held, client)
	f.freed = append(f.freed, client)
	return true
}

func (f *fakePorts) wasFreed(client string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.freed {
		if c == client {
			return true
		}
	}
	return false
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newRegistry(t *testing.T) (*Registry, *fakePorts, *clock) {
	t.Helper()
	ports, c := newFakePorts(), &clock{t: time.Unix(1_700_000_000, 0)}
	return &Registry{Ports: ports, Max: 2, Grace: time.Minute, now: c.now}, ports, c
}

// hostRun attaches a run and gives it a port, as a hosting connection does.
func hostRun(t *testing.T, r *Registry, client string) (int, func()) {
	t.Helper()
	release, err := r.Attach("alice", client)
	if err != nil {
		t.Fatal(err)
	}
	port, err := r.Port("alice", client)
	if err != nil {
		t.Fatal(err)
	}
	return port, release
}

func (r *Registry) state(client string) (State, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ru, ok := r.runs[key{"alice", client}]
	if !ok {
		return 0, false
	}
	return ru.state, true
}

func TestAReattachWithinGraceKeepsThePort(t *testing.T) {
	r, ports, c := newRegistry(t)
	port, release := hostRun(t, r, "0123abcd")
	release()
	if s, _ := r.state("0123abcd"); s != Grace {
		t.Fatalf("after its last connection the run is %v, want grace", s)
	}

	c.advance(59 * time.Second)
	r.Sweep(t.Context())
	again, _ := hostRun(t, r, "0123abcd")
	if again != port {
		t.Errorf("the reattached run got port %d, want %d", again, port)
	}
	if s, _ := r.state("0123abcd"); s != Live || ports.wasFreed("0123abcd") {
		t.Errorf("the reattached run is %v, freed %v; want live and held", s, ports.wasFreed("0123abcd"))
	}
}

func TestARunExpiresAfterGrace(t *testing.T) {
	r, ports, c := newRegistry(t)
	_, release := hostRun(t, r, "0123abcd")
	release()
	release() // a second call counts nothing

	c.advance(time.Minute)
	r.Sweep(t.Context())
	if _, ok := r.state("0123abcd"); ok || !ports.wasFreed("0123abcd") {
		t.Errorf("an expired run: still known %v, port freed %v", ok, ports.wasFreed("0123abcd"))
	}
}

// One live connection keeps the run, however many others end.
func TestARunWithAConnectionDoesNotExpire(t *testing.T) {
	r, _, c := newRegistry(t)
	_, hosting := hostRun(t, r, "0123abcd")
	query, err := r.Attach("alice", "0123abcd")
	if err != nil {
		t.Fatal(err)
	}
	query()
	c.advance(time.Hour)
	r.Sweep(t.Context())
	if s, ok := r.state("0123abcd"); !ok || s != Live {
		t.Errorf("a run with a connection is %v (known %v), want live", s, ok)
	}
	hosting()
}

func TestTheLimitCountsGraceRuns(t *testing.T) {
	r, _, _ := newRegistry(t)
	_, a := hostRun(t, r, "0123abcd")
	_, b := hostRun(t, r, "4567cdef")
	a()
	b()

	_, err := r.Attach("alice", "89abcdef")
	if err == nil {
		t.Fatal("a third run was attached beside two in grace")
	}
	want := "ephemeral: account alice has 2 clients\n  fix: raise WORKSPACE_EPHEMERAL_MAX_CLIENTS or wait"
	if err.Error() != want {
		t.Errorf("refusal = %q, want %q", err, want)
	}
	if release, err := r.Attach("alice", "0123abcd"); err != nil {
		t.Errorf("a run in grace was refused its reattach: %v", err)
	} else {
		release()
	}
	if release, err := r.Attach("bob", "89abcdef"); err != nil {
		t.Errorf("another account was refused: %v", err)
	} else {
		release()
	}
}

// A run that never bound a port leaves nothing behind, so it holds no slot.
func TestAPortlessRunEndsWithItsConnection(t *testing.T) {
	r, _, _ := newRegistry(t)
	for range 3 {
		release, err := r.Attach("alice", "0123abcd")
		if err != nil {
			t.Fatal(err)
		}
		release()
	}
	if _, ok := r.state("0123abcd"); ok {
		t.Error("a run with no port outlived its connection")
	}
}

// The port is freed only after Cleanup says nothing names it, so it is never
// handed to another run while a volume still does (ADR 0032).
func TestThePortOutlivesAFailedCleanup(t *testing.T) {
	r, ports, c := newRegistry(t)
	cleaned := errors.New("a volume still names the port")
	r.Cleanup = func(context.Context, string, string) error { return cleaned }

	_, release := hostRun(t, r, "0123abcd")
	release()
	c.advance(time.Minute)
	r.Sweep(t.Context())
	if s, _ := r.state("0123abcd"); s != Cleaning || ports.wasFreed("0123abcd") {
		t.Fatalf("after a failed cleanup the run is %v, port freed %v; want cleaning and held", s, ports.wasFreed("0123abcd"))
	}
	if _, err := r.Attach("alice", "0123abcd"); err == nil || !strings.Contains(err.Error(), "cleaned up") {
		t.Errorf("a run being cleaned was attached: %v", err)
	}

	cleaned = nil
	r.Sweep(t.Context())
	if _, ok := r.state("0123abcd"); ok || !ports.wasFreed("0123abcd") {
		t.Error("the next sweep did not finish the cleanup")
	}
}

func TestTheRecordSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	r, _, c := newRegistry(t)
	r.Dir = dir
	livePort, _ := hostRun(t, r, "0123abcd") // live when the agent stops
	gracePort, release := hostRun(t, r, "4567cdef")
	release()
	c.advance(50 * time.Second)

	restarted, ports, _ := newRegistry(t)
	restarted.Dir, restarted.now = dir, c.now
	if err := restarted.Restore(); err != nil {
		t.Fatal(err)
	}
	if got, ok := ports.held["0123abcd"]; !ok || ports.ports["0123abcd"] != livePort || got == 0 {
		t.Errorf("the live run's port %d was not held again: %v", livePort, ports.ports)
	}

	// Ten more seconds: the run in grace has had its minute, the live one has
	// had ten seconds of it.
	c.advance(10 * time.Second)
	restarted.Sweep(t.Context())
	if _, ok := restarted.state("4567cdef"); ok || !ports.wasFreed("4567cdef") {
		t.Errorf("the run in grace (port %d) outlived its deadline across the restart", gracePort)
	}
	again, _ := hostRun(t, restarted, "0123abcd")
	if again != livePort {
		t.Errorf("the live run reattached on %d, want %d", again, livePort)
	}
}

// Live and grace runs both count against the limit after a restart.
func TestARestoredRunCountsAgainstTheLimit(t *testing.T) {
	dir := t.TempDir()
	r, _, _ := newRegistry(t)
	r.Dir = dir
	hostRun(t, r, "0123abcd")
	hostRun(t, r, "4567cdef")

	restarted, _, _ := newRegistry(t)
	restarted.Dir = dir
	if err := restarted.Restore(); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Attach("alice", "89abcdef"); err == nil {
		t.Error("the limit forgot the runs restored from the record")
	}
}
