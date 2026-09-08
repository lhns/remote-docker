package machine

// Locating and holding a machine, against a backend that records what was asked
// of it.
//
// This is the part that cost five CI rounds to get right, and every one of the
// mistakes was an ordering or a lifetime rather than a platform detail -- which
// is to say all of them were reachable from here. The measurements behind the
// assertions are in ADR 0026; what is pinned here is that the code still does
// what they concluded.

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// listening opens a socket for the duration of a test and returns its port, so
// a located machine has something to answer the dial Locate makes.
func listening(t *testing.T) int {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l.Addr().(*net.TCPAddr).Port
}

// nothingListening returns a port with nothing behind it.
func nothingListening(t *testing.T) int {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

// fakeBackend records the calls made to it and answers as told.
type fakeBackend struct {
	name string

	calls    []string
	address  string
	startErr error
	addrErr  error

	held   int
	closed int
}

func (f *fakeBackend) Name() string { return f.name }

func (f *fakeBackend) Available(context.Context) error { return nil }

func (f *fakeBackend) Inspect(context.Context, string) (Observed, error) {
	f.calls = append(f.calls, "inspect")
	return Observed{State: Stopped}, nil
}

func (f *fakeBackend) Create(context.Context, Spec) error {
	f.calls = append(f.calls, "create")
	return nil
}

func (f *fakeBackend) Enrol(context.Context, string, string, string) error {
	f.calls = append(f.calls, "enrol")
	return nil
}

func (f *fakeBackend) Start(context.Context, string) error {
	f.calls = append(f.calls, "start")
	return f.startErr
}

func (f *fakeBackend) Hold(context.Context, string) (io.Closer, error) {
	f.calls = append(f.calls, "hold")
	f.held++
	return closerFunc(func() error {
		f.closed++
		return nil
	}), nil
}

func (f *fakeBackend) Address(context.Context, string) (string, error) {
	f.calls = append(f.calls, "address")
	return f.address, f.addrErr
}

func (f *fakeBackend) Stop(context.Context, string) error {
	f.calls = append(f.calls, "stop")
	return nil
}

func (f *fakeBackend) Destroy(context.Context, string) error {
	f.calls = append(f.calls, "destroy")
	return nil
}

// register puts a fake in the backend list for one test and takes it out again.
func register(t *testing.T, f *fakeBackend) {
	t.Helper()

	before := registered
	registered = append(append([]Backend{}, registered...), f)
	t.Cleanup(func() { registered = before })
}

func TestLocateStartsBeforeAsking(t *testing.T) {
	fake := &fakeBackend{name: "fake", address: "127.0.0.1"}
	register(t, fake)

	got, err := Locate(context.Background(), "fake", "dev", listening(t))
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if got != "127.0.0.1" {
		t.Errorf("Locate = %q", got)
	}

	// The order is the whole point. A stopped machine has no address, so asking
	// first answers nothing and starting first is what makes the question
	// answerable -- and starting a running machine is what keeps a WSL
	// distribution from idling out from under the caller.
	if len(fake.calls) != 2 || fake.calls[0] != "start" || fake.calls[1] != "address" {
		t.Errorf("Locate did %v, want [start address]", fake.calls)
	}
}

func TestLocateRefusesAMachineWithNoAddress(t *testing.T) {
	// A machine that is up but has not been given an address yet -- DHCP has not
	// finished, or the integration services have not reported. Returning the
	// empty string here would have the caller dial ":2222", which connects to
	// something on this computer or fails saying nothing about the machine.
	fake := &fakeBackend{name: "fake", address: ""}
	register(t, fake)

	if _, err := Locate(context.Background(), "fake", "dev", 2222); err == nil {
		t.Fatal("a machine with no address was reported as reachable")
	}
}

func TestLocateReportsAMachineThatWillNotStart(t *testing.T) {
	fake := &fakeBackend{name: "fake", startErr: errors.New("no such distribution")}
	register(t, fake)

	_, err := Locate(context.Background(), "fake", "dev", 2222)
	if err == nil {
		t.Fatal("a machine that would not start was located anyway")
	}
	// Named, because the caller is about to be told a connection was refused
	// and the reason is two layers down.
	if !strings.Contains(err.Error(), "dev") || !strings.Contains(err.Error(), "no such distribution") {
		t.Errorf("the error says neither which machine nor why: %v", err)
	}
	// And it did not go on to ask an unstarted machine where it is.
	for _, c := range fake.calls {
		if c == "address" {
			t.Error("a machine that failed to start was asked for its address")
		}
	}
}

// A located machine is one that can be dialled.
//
// The bug this pins: Locate started the machine, returned its address and let
// the caller dial immediately. A machine that was stopped is UP before its
// agent is -- the agent generates a host key and waits for dockerd before it
// listens -- so the first command after a machine had been left alone failed
// with a refused connection and the second worked, which is how a deterministic
// failure comes to look like a flaky one.
func TestLocateWaitsForTheAgent(t *testing.T) {
	port := nothingListening(t)
	fake := &fakeBackend{name: "fake", address: "127.0.0.1"}
	register(t, fake)

	// Shortened, or this test would take the three minutes a real machine is
	// allowed. The duration itself is argued for where it is declared.
	restore := AgentStartTimeout
	AgentStartTimeout = 2 * time.Second
	t.Cleanup(func() { AgentStartTimeout = restore })

	_, err := Locate(context.Background(), "fake", "dev", port)
	if err == nil {
		t.Fatal("a machine whose agent never answered was located anyway")
	}
	// The machine, the address and the port, because the caller is otherwise
	// told only that a connection was refused.
	for _, want := range []string{"dev", "127.0.0.1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %q: %v", want, err)
		}
	}
}

// And a machine whose agent answers is returned without waiting the timeout out.
func TestLocateReturnsAsSoonAsTheAgentAnswers(t *testing.T) {
	fake := &fakeBackend{name: "fake", address: "127.0.0.1"}
	register(t, fake)

	restore := AgentStartTimeout
	AgentStartTimeout = time.Minute
	t.Cleanup(func() { AgentStartTimeout = restore })

	start := time.Now()
	if _, err := Locate(context.Background(), "fake", "dev", listening(t)); err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if waited := time.Since(start); waited > 10*time.Second {
		t.Errorf("Locate waited %s for a machine that was already answering", waited)
	}
}

func TestHoldIsReleasedExactlyOnce(t *testing.T) {
	fake := &fakeBackend{name: "fake", address: "10.0.0.2"}
	register(t, fake)

	hold, err := Hold(context.Background(), "fake", "dev")
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if fake.held != 1 {
		t.Errorf("Hold held %d times", fake.held)
	}
	if fake.closed != 0 {
		t.Error("the hold was released before anyone asked")
	}

	if err := hold.Close(); err != nil {
		t.Errorf("closing the hold: %v", err)
	}
	if fake.closed != 1 {
		t.Errorf("closing the hold released it %d times", fake.closed)
	}
}

func TestLocateAndHoldNameAnUnknownBackend(t *testing.T) {
	// Every path to a machine goes through one of these two, so a workspace
	// naming a backend this build does not have must say so here rather than
	// failing later as a connection error.
	if _, err := Locate(context.Background(), "nonesuch", "dev", 2222); err == nil {
		t.Error("Locate accepted a backend that does not exist")
	}
	if _, err := Hold(context.Background(), "nonesuch", "dev"); err == nil {
		t.Error("Hold accepted a backend that does not exist")
	}
}
