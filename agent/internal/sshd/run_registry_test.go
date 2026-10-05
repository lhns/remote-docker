package sshd

import (
	"fmt"
	"net"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/lhns/remote-docker/agent/internal/ephemeral"
)

// The run registry behind the SSH conversation (ADR 0050): its ports, its
// grace period and its limit.

// runID is a distinct, valid run id for each i.
func runID(i int) string { return fmt.Sprintf("%032x", i+1) }

// hostAgain binds a run's port once the workspace has noticed the previous
// hosting connection end, which releases the reservation asynchronously.
func hostAgain(t *testing.T, c *ssh.Client, port int) net.Listener {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		l, err := host(c, port)
		if err == nil {
			return l
		}
		if time.Now().After(deadline) {
			t.Fatalf("the run was refused its port %d again: %v", port, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A run's port lives in the agent's memory only, so clientports does not grow
// by a line per run for ever.
func TestARunsPortIsNotRecorded(t *testing.T) {
	w := startRunWorkspace(t)
	l, err := host(w.dial(t, "alice", w.alice, runA), 0)
	if err != nil {
		t.Fatalf("the run was refused its forward: %v", err)
	}
	defer func() { _ = l.Close() }()
	if w.recorded(t, "alice", w.run(runA)) {
		t.Error("the run's port was written to clientports")
	}
}

// Runs whose connections have ended count until their grace runs out, and
// reattach on their own port. A new run past the limit is refused at once,
// with the reason and the fix.
func TestTheRunLimitCountsRunsInGrace(t *testing.T) {
	w := startRunWorkspace(t)
	first := 0
	for i := range ephemeral.DefaultMax {
		c := w.dial(t, "alice", w.alice, runID(i))
		l, err := host(c, 0)
		if err != nil {
			t.Fatalf("run %d was refused its forward: %v", i, err)
		}
		if i == 0 {
			first = l.Addr().(*net.TCPAddr).Port
		}
		_ = c.Close()
	}

	started := time.Now()
	ok, why := sendRun(t, w.dial(t, "alice", w.alice, ""), runID(ephemeral.DefaultMax))
	want := fmt.Sprintf("ephemeral: account alice has %d clients\n  fix: raise WORKSPACE_EPHEMERAL_MAX_CLIENTS or wait", ephemeral.DefaultMax)
	if ok || why != want {
		t.Errorf("a run past the limit: ok=%v, %q; want %q", ok, why, want)
	}
	if took := time.Since(started); took > 5*time.Second {
		t.Errorf("the refusal took %v", took)
	}

	_ = hostAgain(t, w.dial(t, "alice", w.alice, runID(0)), first).Close()
}
