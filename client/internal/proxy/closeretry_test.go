package proxy

// Closing a listener that swallows the first close.
//
// go-winio's pipe listener can consume the signal Close sends it and not
// recognise it as a close (microsoft/go-winio#85), leaving Close waiting for a
// listener that has gone back to waiting for a signal. Both sides wait forever,
// and Accept never returns either, so a session hangs behind it. Signalling
// again is what breaks it.
//
// The listener is stood in for here, because reproducing the real race needs
// Windows, a named pipe and a client connecting inside the same microsecond.
// What is tested is our half: that a second close is sent, and that a listener
// which will never close does not take the caller down with it.

import (
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// swallower closes on the Nth call and hangs on every one before it, which is
// the shape of the bug: the earlier signals are consumed and not acted on.
type swallower struct {
	net.Listener
	needed  int32
	calls   atomic.Int32
	release chan struct{}
}

func (s *swallower) Close() error {
	if s.calls.Add(1) < s.needed {
		<-s.release // as the real one does: waits to be told it finished
		return nil
	}
	close(s.release)
	return nil
}

// realListener is a live listener for the stubs to embed, so Addr and anything
// else the code under test reaches for is a real answer rather than a nil.
func realListener(t *testing.T) net.Listener {
	t.Helper()
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inner.Close() })
	return inner
}

func lockedFor(t *testing.T, inner net.Listener) *lockedListener {
	t.Helper()
	return &lockedListener{Listener: inner, lock: &Lock{}}
}

func TestCloseSignalsAgainWhenTheFirstIsSwallowed(t *testing.T) {
	inner := &swallower{Listener: realListener(t), needed: 2, release: make(chan struct{})}
	l := lockedFor(t, inner)

	done := make(chan error, 1)
	go func() { done <- l.Close() }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
		if got := inner.calls.Load(); got < 2 {
			t.Errorf("closed after %d attempts, so the retry never happened", got)
		}
	case <-time.After(closeRetry * closeAttempts * 2):
		t.Fatal("Close never returned, which is the deadlock this exists to break")
	}
}

// A listener that will never close must not hold the caller for ever. The
// endpoint stays bound until the process exits; the alternative is a hang.
func TestCloseGivesUpRatherThanHanging(t *testing.T) {
	// More attempts than are allowed, so it is never satisfied.
	inner := &swallower{Listener: realListener(t), needed: closeAttempts + 10, release: make(chan struct{})}
	l := lockedFor(t, inner)

	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- l.Close() }()

	select {
	case err := <-done:
		if err == nil {
			t.Error("a listener that never closed was reported as closed")
		}
		if took := time.Since(start); took < closeRetry {
			t.Errorf("gave up after %v, before even one retry", took)
		}
	case <-time.After(closeRetry*closeAttempts + 5*time.Second):
		t.Fatal("Close never gave up")
	}
}

// The ordinary case, which is every listener on every other platform: one
// attempt, no timer, no delay.
func TestAnOrdinaryCloseIsImmediate(t *testing.T) {
	inner := &swallower{Listener: realListener(t), needed: 1, release: make(chan struct{})}
	l := lockedFor(t, inner)

	start := time.Now()
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if took := time.Since(start); took >= closeRetry {
		t.Errorf("an immediate close took %v", took)
	}
	if got := inner.calls.Load(); got != 1 {
		t.Errorf("closed %d times, want 1", got)
	}
}

// The error from the listener is the caller's to see.
func TestCloseReturnsTheListenersError(t *testing.T) {
	want := errors.New("no")
	l := lockedFor(t, failingListener{Listener: realListener(t), err: want})
	if err := l.Close(); !errors.Is(err, want) {
		t.Errorf("Close() = %v, want %v", err, want)
	}
}

type failingListener struct {
	net.Listener
	err error
}

func (f failingListener) Close() error { return f.err }
