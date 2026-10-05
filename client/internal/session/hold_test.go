package session

import (
	"context"
	"testing"
	"time"

	"github.com/lhns/remote-docker/client/internal/config"
	"github.com/lhns/remote-docker/core/workspace"
)

// An ephemeral run holds its connection for the life of the process: released,
// it would start the run's grace period and expire it under a live process
// (ADR 0050). A machine is released when idle, as ever (ADR 0015).
func TestOnlyAMachineIsReleasedWhenIdle(t *testing.T) {
	for _, c := range []struct {
		name, client string
		released     bool
	}{
		{"machine", "", true},
		{"ephemeral run", "0123abcd", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, err := Open(context.Background(), Options{
				Config:      config.Config{Host: "workspace.invalid", User: "alice", Port: 22},
				Role:        Query,
				IdleTimeout: time.Millisecond,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })

			s.gate.open = func(context.Context) (*liveConn, error) {
				return &liveConn{info: workspace.Info{User: "alice", Client: c.client}}, nil
			}
			s.gate.shut = func(*liveConn) {}
			s.gate.busy = func(context.Context, *liveConn) (bool, error) { return false, nil }
			s.gate.alive = nil

			_, done, err := s.gate.acquire(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			done()
			time.Sleep(5 * time.Millisecond)

			if got := s.gate.sweep(t.Context()); got != c.released {
				t.Errorf("idle %s released: %v, want %v", c.name, got, c.released)
			}
		})
	}
}
