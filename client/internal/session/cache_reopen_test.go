package session

import (
	"context"
	"testing"
	"time"

	"github.com/lhns/remote-docker/core/workspace"
	"github.com/lhns/remote-docker/dircache"
)

// The cache channel was opened once per connection and kept for good, so
// after one timeout (which closes the stream) or one end of the stream by the
// workspace, every Changes, Pull and Apply failed until the SSH connection was
// replaced, which a running container can keep from ever happening (ADR 0044).
//
// The fake workspace answers the greeting and ends the stream, which is what an
// agent restarting its handler looks like from here.
func TestACacheChannelTheWorkspaceEndedIsReopened(t *testing.T) {
	client := startWorkspace(t, func(string) (string, bool) { return helloFor(t), true })
	s := &Session{}
	live := &liveConn{ssh: client}

	first, err := s.ensureCacheChan(t.Context(), live)
	if err != nil {
		t.Fatalf("opening the channel: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := first.Changes(ctx, "/m/1111111111111111"); err == nil {
		t.Fatal("a stream the workspace ended answered a request")
	}

	second, err := s.ensureCacheChan(t.Context(), live)
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	if second == first {
		t.Error("the next request was handed the channel that had just failed; it must open another")
	}
}

// The other way a channel dies: a timed-out exchange closes its own stream, so
// the next caller does not read from the middle of a tar.
func TestACacheChannelATimeoutClosedIsReopened(t *testing.T) {
	client := startWorkspace(t, func(string) (string, bool) { return helloFor(t), true })
	s := &Session{}
	live := &liveConn{ssh: client}

	first, err := s.ensureCacheChan(t.Context(), live)
	if err != nil {
		t.Fatalf("opening the channel: %v", err)
	}
	_ = first.Close()

	second, err := s.ensureCacheChan(t.Context(), live)
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	if second == first {
		t.Error("a closed channel was handed out again")
	}
}

// A share restored from the record after a restart registers for NFS and
// told the cache nothing, so write-back, which polls only attached shares,
// never ran for it again (ADR 0027, ADR 0044).
func TestARestoredUnionShareIsAttachedToTheCache(t *testing.T) {
	st, project, _ := store(t)
	mode := workspace.Mode{Read: workspace.ReadCached, Write: workspace.WriteBack}
	share := shareWith(t, project, mode, workspace.ShareWords{Watch: workspace.WatchPartial, Prefetch: workspace.PrefetchOff})
	st.remember(share)

	s := hostingSession(t, workspace.WatchOff, newShareStore(st.path, nil))
	s.cache = &dircache.Cache{Store: func() (dircache.Store, bool) { return nil, false }, Ctx: t.Context()}
	t.Cleanup(s.cache.Stop)

	if _, _, ok := s.registry.LookupOrRestore(share.ExportPath); !ok {
		t.Fatal("the share was not restored")
	}

	for _, r := range s.cache.Reports() {
		if r.Local == project {
			return
		}
	}
	t.Errorf("a restored write=back share is not attached to the cache; reports = %+v", s.cache.Reports())
}
