package rewrite

import (
	"context"
	"testing"
	"time"

	"github.com/lhns/remote-docker/pkg/workspace"
)

// The bug this guards against costs a container its filesystem and says
// nothing: the collector deletes a volume that exists but is not yet named by
// any container, and the daemon then RECREATES it as an empty local volume
// when the container starts. `remote-docker start && docker run -v $PWD:/w`
// failed in CI with the project directory mounted empty.
func TestCollectSparesAVolumeThisSessionIsExporting(t *testing.T) {
	name, err := workspace.VolumeNameForExport("", workspace.ExportCWD)
	if err != nil {
		t.Fatalf("naming the cwd volume: %v", err)
	}

	store := &fakeVolumeStore{volumes: []Volume{managed(name, "alice")}}
	c := newCollector(store, "alice")
	c.Guard = &Guard{Exported: func(v string) bool { return v == name }}

	removed, err := c.Collect(t.Context())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if removed != 0 {
		t.Errorf("removed %d, want 0", removed)
	}
	if len(store.removed) != 0 {
		t.Errorf("removed %v, want nothing", store.removed)
	}
}

// blockingVolumes runs a hook in the middle of EnsureVolume, which is the
// moment the volume exists and no container names it yet.
type blockingVolumes struct {
	inside func()
}

func (b *blockingVolumes) EnsureVolume(context.Context, string, map[string]string, map[string]string) error {
	if b.inside != nil {
		b.inside()
	}
	return nil
}

// A rewrite in progress holds the guard from registering the share to creating
// the volume, so a collection starting in that window waits rather than
// deciding on a half-built world.
//
// The share registration is what saves the volume, and it happens INSIDE the
// held section, so without the lock a collector that read the registry a
// moment earlier would still delete it.
func TestARewriteInProgressBlocksCollection(t *testing.T) {
	name, err := workspace.VolumeNameForExport("", workspace.ExportCWD)
	if err != nil {
		t.Fatalf("naming the cwd volume: %v", err)
	}

	exported := make(chan struct{})
	guard := &Guard{Exported: func(v string) bool {
		select {
		case <-exported:
			return v == name
		default:
			return false
		}
	}}

	store := &fakeVolumeStore{volumes: []Volume{managed(name, "alice")}}
	c := newCollector(store, "alice")
	c.Guard = guard

	collected := make(chan int, 1)
	volumes := &blockingVolumes{inside: func() {
		// The share is registered by now; a real Sharer does it one line
		// above. Collection must not have got past the guard.
		close(exported)
		go func() {
			n, _ := c.Collect(context.Background())
			collected <- n
		}()
		select {
		case <-collected:
			t.Error("collection ran while a rewrite held the guard")
		case <-time.After(50 * time.Millisecond):
		}
	}}

	r := &Rewriter{Shares: &fakeSharer{cwd: "/home/alice/project"}, Volumes: volumes, NFSPort: 30000, Guard: guard}
	body := []byte(`{"HostConfig":{"Binds":["/home/alice/project:/w"]}}`)
	if _, err := r.ContainerCreate(t.Context(), body); err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}

	select {
	case n := <-collected:
		if n != 0 {
			t.Errorf("collection removed %d volumes after the rewrite, want 0", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("collection never finished after the rewrite released the guard")
	}
	if len(store.removed) != 0 {
		t.Errorf("removed %v, want nothing", store.removed)
	}
}
