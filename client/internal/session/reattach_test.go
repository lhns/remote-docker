package session

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/lhns/remote-docker/core/cache"
	"github.com/lhns/remote-docker/core/workspace"
	"github.com/lhns/remote-docker/dircache"
)

// Only unions are worth asking the cache channel about, so a session whose
// record holds none never opens it and stays silent against a workspace too old
// for it.
func TestOnlyRecordedUnionsAreReattached(t *testing.T) {
	st, project, _ := store(t)
	through := shareWith(t, project, workspace.DefaultMode, workspace.ShareWords{})
	st.remember(through)

	if got := st.unionExports(); len(got) != 0 {
		t.Errorf("a write=through share is listed for the cache: %v", got)
	}

	other := t.TempDir()
	union := shareWith(t, other, workspace.Mode{Read: workspace.ReadCached, Write: workspace.WriteBack}, workspace.ShareWords{Watch: workspace.WatchPartial})
	st.remember(union)
	if got := st.unionExports(); !slices.Equal(got, []string{union.ExportPath}) {
		t.Errorf("unionExports = %v, want only %s", got, union.ExportPath)
	}
}

// A client restarted under a running container restores the unions the workspace
// reports mounted at connect, with no NFS traffic to prompt it: a container that
// only writes into its union presents no mount and no root handle, and write-back
// for it would otherwise never run again.
func TestAConnectRestoresTheUnionsTheWorkspaceHasMounted(t *testing.T) {
	st, project, _ := store(t)
	mode := workspace.Mode{Read: workspace.ReadCached, Write: workspace.WriteBack}
	share := shareWith(t, project, mode, workspace.ShareWords{Watch: workspace.WatchPartial, Prefetch: workspace.PrefetchOff})
	st.remember(share)

	// A union recorded whose container is gone: the workspace holds no cache for it.
	gone := shareWith(t, t.TempDir(), mode, workspace.ShareWords{Watch: workspace.WatchPartial, Prefetch: workspace.PrefetchOff})
	st.remember(gone)

	s := hostingSession(t, workspace.WatchOff, newShareStore(st.path, nil))
	s.cache = &dircache.Cache{Store: func() (dircache.Store, bool) { return nil, false }, Ctx: t.Context()}
	t.Cleanup(s.cache.Stop)

	const client = "aabbccdd"
	volume, err := workspace.CacheVolumeForExport(client, share.ExportPath)
	if err != nil {
		t.Fatal(err)
	}
	reply, err := json.Marshal(cache.Reply{Caches: []string{volume}})
	if err != nil {
		t.Fatal(err)
	}
	stream := newScripted(string(reply) + "\n")
	live := &liveConn{clientID: client, cacheChan: channelOver(stream)}

	s.reattachUnions(t.Context(), live)

	var attached []string
	for _, r := range s.cache.Reports() {
		attached = append(attached, r.Local)
	}
	if !slices.Equal(attached, []string{project}) {
		t.Errorf("attached %v, want only the union the workspace reported mounted (%s)", attached, project)
	}
}

// A share is recorded before its volume exists, so a prune must not forget the
// record of one this session has registered.
func TestAPruneKeepsTheRecordOfAShareWithNoVolumeYet(t *testing.T) {
	st, project, export := store(t)
	st.remember(shareOf(t, project))

	keep := recordsToKeep(nil, "aabbccdd", []string{export})
	st.forget(keep)
	if _, _, _, ok := st.restore(export); !ok {
		t.Error("the record of a registered share was forgotten for want of a volume")
	}

	st.forget(recordsToKeep(nil, "aabbccdd", nil))
	if _, _, _, ok := st.restore(export); ok {
		t.Error("the record of a share with no volume and no registration was kept")
	}
}
