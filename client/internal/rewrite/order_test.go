package rewrite

import (
	"context"
	"testing"

	"github.com/lhns/remote-docker/core/workspace"
)

// A mount that is refused leaves nothing behind: a workspace that cannot serve
// the union is found out before the cache channel is opened or the watcher is
// started, and a second mount of a directory disagreeing with the first never
// starts anything for itself.
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

	r, _, _, started = watchRewriter(workspace.WatchOff)
	body := []byte(`{"HostConfig":{"Binds":["` + watchProject + `:/a:watch=partial","` + watchProject + `:/b:watch=coarse"]}}`)
	if _, err := r.ContainerCreate(t.Context(), body); err == nil {
		t.Fatal("two mounts of one directory with different words were accepted")
	}
	if *started != 1 {
		t.Errorf("the watcher was started %d times, want only the first mount's", *started)
	}
}
