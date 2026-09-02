package session

import (
	"context"
	"testing"
	"time"
)

// standbySession is the least Session that Standby and wake can run against:
// no watcher and no connection, which is also the shape of a session that
// stood by before anything ever used it.
func standbySession(t *testing.T) *Session {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	s := &Session{ctx: ctx, cancel: cancel}
	s.gate = &connGate[*liveConn]{
		open: func(context.Context) (*liveConn, error) { return nil, context.Canceled },
		shut: func(*liveConn) {},
	}
	return s
}

// Standby and waking are a state, not an event: a session stands by, is woken
// by a request, and stands by again, any number of times.
func TestStandbyAndWakeAreRepeatable(t *testing.T) {
	s := standbySession(t)

	if s.isDormant() {
		t.Fatal("a new session is already dormant")
	}

	for round := 1; round <= 3; round++ {
		s.Standby()
		if !s.isDormant() {
			t.Fatalf("round %d: not dormant after Standby", round)
		}
		s.wake()
		if s.isDormant() {
			t.Fatalf("round %d: still dormant after wake", round)
		}
	}
}

// Both are idempotent, which is what lets the poll behind them carry no state:
// it may call Standby on every tick, and every request calls wake.
func TestStandbyAndWakeAreIdempotent(t *testing.T) {
	s := standbySession(t)

	s.Standby()
	s.Standby()
	if !s.isDormant() {
		t.Error("a second Standby undid the first")
	}

	s.wake()
	s.wake()
	if s.isDormant() {
		t.Error("a second wake undid the first")
	}
}

// The share reconciler runs on its own ticker for the life of the session and
// syncs the registry's shares to the watcher. Left alone it would put back the
// watches Standby had just dropped, on its next tick.
//
// Asserted by running it: this session has no watcher, so a tick that does not
// skip dereferences a nil one and takes the test process with it. Remove the
// dormant check and this fails rather than passing quietly with the watches
// silently restored.
func TestTheShareReconcilerLeavesStandbyAlone(t *testing.T) {
	s := standbySession(t)
	s.Standby()

	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.reconcileShares(ctx, 10*time.Millisecond)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the reconciler did not stop with its context")
	}
}
