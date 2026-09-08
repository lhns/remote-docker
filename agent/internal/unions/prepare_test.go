package unions

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lhns/remote-docker/core-agent/union"
	"github.com/lhns/remote-docker/core/cache"
	"github.com/lhns/remote-docker/core/workspace"
)

// The machine asking, and two shares of its own. Both exports are share ids
// rather than /cwd, so CacheVolumeForExport can name a volume for them.
const (
	thisClient = "aabbccdd"
	coldExport = "/m/00112233445566ff"
	warmExport = "/m/ffeeddccbbaa9988"
)

// manager is one with nothing mounted, a cache volume directory that satisfies
// union.Spec.Validate, and a log nobody reads: the supervisor these tests start
// cannot exec anything on a development machine and says so every two seconds.
func manager(t *testing.T) *Manager {
	t.Helper()
	return &Manager{
		Volumes: fakeVolumes{mountpoint: "/var/lib/docker/volumes/rd-cache/_data"},
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		shares:  map[string]*live{},
	}
}

// prepareFor is what the client sends for one of this machine's shares.
func prepareFor(t *testing.T, export string) cache.Request {
	t.Helper()
	vol, err := workspace.CacheVolumeForExport(thisClient, export)
	if err != nil {
		t.Fatal(err)
	}
	return cache.Request{
		Op: cache.OpPrepare, Export: export, Port: 30001,
		Cache: vol, Read: string(workspace.ReadCached),
	}
}

// hold puts a union in the manager's map without starting anything.
func hold(m *Manager, account, export string) {
	done := make(chan struct{})
	close(done)
	m.mu.Lock()
	m.shares[key(account, export)] = &live{
		spec:   union.Spec{Export: export, Port: 30001, CacheDir: "/var/lib/docker/volumes/rd-cache/_data"},
		cancel: func() {},
		done:   done,
	}
	m.mu.Unlock()
}

// A cold union belongs to one share, not to the manager.
//
// Prepare used to hold m.mu from before the liveness check to after waitReady,
// which is up to readyTimeout (90s). Every other cache operation reaches
// m.share and takes the same mutex, so one account waiting for its union to
// mount stalled every other account's cache channel.
func TestPrepareDoesNotStallAnotherShare(t *testing.T) {
	m := manager(t)
	hold(m, "bob", warmExport)

	m.probe = func(_ context.Context, spec union.Spec) error {
		if spec.Export == coldExport {
			// Mounting, and not there yet, which is what keeps Prepare in
			// waitReady for the whole of readyTimeout.
			return errors.New("union: not up yet")
		}
		return nil
	}

	mounting := make(chan union.Spec, 1)
	m.onStart = func(spec union.Spec) { mounting <- spec }

	ctx, cancel := context.WithCancel(context.Background())
	prepared := make(chan struct{})
	go func() {
		defer close(prepared)
		_, _ = m.Prepare(ctx, "alice", thisClient, Daemon{}, prepareFor(t, coldExport))
	}()

	select {
	case <-mounting:
	case <-time.After(10 * time.Second):
		t.Fatal("Prepare never reached the mount")
	}

	answered := make(chan error, 1)
	go func() { answered <- m.Drop(context.Background(), "bob", warmExport, nil) }()

	select {
	case err := <-answered:
		if err != nil {
			t.Errorf("the other share's Drop failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("a Drop for one share waited on another share's Prepare")
	}

	cancel()
	<-prepared
}

// Two Prepares for one share mount one union.
//
// A regression guard rather than a bug found: on the single mutex this
// replaced, the lock itself made Prepare exclusive. The single-flight has to
// keep that, because the two would otherwise stack a second fuse-overlayfs on
// one upper and work directory, which overlayfs refuses (ADR 0044).
func TestPrepareStartsOneUnionForConcurrentRequests(t *testing.T) {
	m := manager(t)

	var starts atomic.Int64
	m.onStart = func(union.Spec) { starts.Add(1) }
	m.probe = func(context.Context, union.Spec) error {
		if starts.Load() == 0 {
			return errors.New("union: nothing is mounted there")
		}
		return nil
	}

	req := prepareFor(t, coldExport)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.Prepare(context.Background(), "alice", thisClient, Daemon{}, req); err != nil {
				t.Errorf("Prepare: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := starts.Load(); got != 1 {
		t.Errorf("two Prepares for one share started %d unions", got)
	}
}

// A wedged FUSE server answers nothing, and the cache session's context has no
// deadline to stop waiting on it.
//
// gliderlabs builds a session context with context.WithCancel alone
// (agent/internal/sshd/cache.go), so mergedRoot passing it through left an
// Apply or a Drop blocked for as long as the SSH session lived.
func TestApplyGivesUpOnAWedgedUnion(t *testing.T) {
	m := manager(t)
	hold(m, "alice", warmExport)

	release := make(chan struct{})
	defer close(release)
	m.probe = func(ctx context.Context, _ union.Spec) error {
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	answered := make(chan error, 1)
	go func() {
		answered <- m.Apply(context.Background(), "alice", warmExport, cache.CodecNone, strings.NewReader(""))
	}()

	select {
	case err := <-answered:
		if err == nil {
			t.Error("a union that never answered was written to")
		}
	case <-time.After(aliveTimeout + 5*time.Second):
		t.Errorf("Apply waited past %s on a context with no deadline", aliveTimeout)
	}
}
