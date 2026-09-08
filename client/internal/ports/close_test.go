package ports

import (
	"context"
	"testing"
	"time"
)

// blockingDocker holds its answer until the test releases it, which is the gap
// Reconcile leaves open: ListContainers is called outside m.mu, so a Close can
// land while a reconciliation is in flight.
type blockingDocker struct {
	containers []Container
	release    chan struct{}
	called     chan struct{}
}

func (d *blockingDocker) ListContainers(context.Context) ([]Container, error) {
	close(d.called)
	<-d.release
	return d.containers, nil
}

// TestReconcileOpensNothingAfterClose pins that Close ends the manager. It
// closes every forward and empties the map, and a Reconcile that was already
// listing containers then repopulated it -- opening real listeners, and a
// goroutine each for udp, that nothing would ever close: session.liveConn.close
// calls Close exactly once.
func TestReconcileOpensNothingAfterClose(t *testing.T) {
	docker := &blockingDocker{
		containers: []Container{{ID: "a", Name: "web", Ports: []Published{tcp(8080, 80)}}},
		release:    make(chan struct{}),
		called:     make(chan struct{}),
	}
	fwd := newForwarder()
	m := &Manager{Docker: docker, Forwarder: fwd}

	done := make(chan error, 1)
	go func() { done <- m.Reconcile(context.Background()) }()

	<-docker.called
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	close(docker.release)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Reconcile did not return")
	}

	if active := m.Active(); len(active) != 0 {
		t.Fatalf("after Close the manager is forwarding %v", active)
	}

	fwd.mu.Lock()
	opened := append([]string(nil), fwd.opened...)
	fwd.mu.Unlock()
	if len(opened) != 0 {
		t.Fatalf("a forward was opened after Close: %v", opened)
	}
}
