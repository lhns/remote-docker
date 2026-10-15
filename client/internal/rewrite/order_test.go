package rewrite

import (
	"context"
	"testing"

	"github.com/lhns/remote-docker/core/workspace"
)

// A mount that is refused leaves nothing behind: a workspace that cannot serve
// the union is found out before the cache channel is opened or the watcher is
// started, and so is a mount that contradicts itself.
func TestARefusedMountOpensNoChannelAndStartsNoWatcher(t *testing.T) {
	r, _, _, started := watchRewriter(workspace.WatchOff)
	opened := 0
	r.OpenCache = func(context.Context) (Cache, error) { opened++; return &fakeCache{}, nil }
	r.UnionReady = workspace.UnionNoBinary
	if _, err := r.ContainerCreate(t.Context(), bindBody("write=back")); err == nil {
		t.Fatal("a union the workspace cannot serve was accepted")
	}
	if opened != 0 || *started != 0 {
		t.Errorf("a refused union opened the channel %d times and started the watcher %d times", opened, *started)
	}

	// Contradicting itself is still refused: a cached mount cannot watch
	// nothing, and the refusal starts nothing either.
	r, _, _, started = watchRewriter(workspace.WatchOff)
	r.OpenCache = func(context.Context) (Cache, error) { opened++; return &fakeCache{}, nil }
	r.UnionReady = workspace.UnionReady
	if _, err := r.ContainerCreate(t.Context(), bindBody("read=cached,write=back,watch=off")); err == nil {
		t.Fatal("read=cached with watch=off was accepted")
	}
	if opened != 0 || *started != 0 {
		t.Errorf("a contradicting mount opened the channel %d times and started the watcher %d times", opened, *started)
	}
}
