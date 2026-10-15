package session

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/lhns/remote-docker/core-client/nfsserve"
	"github.com/lhns/remote-docker/core/workspace"
)

// hostingSession is a session that can start a watcher on demand: hosting, a
// registry with the session's restore hooks, and no connection.
func hostingSession(t *testing.T, defaultWatch workspace.Watch, shares *shareStore) *Session {
	t.Helper()
	s := standbySession(t)
	s.opts = Options{Role: Host, Watch: defaultWatch}
	s.registry = nfsserve.NewRegistry(defaultAttrs(0, 0))
	s.shares = shares
	s.registry.Restore = s.restoreShare
	s.registry.Restored = s.shareRestored
	t.Cleanup(s.closeWatcher)
	return s
}

// The watch word is recorded with the share, resolved, and a later mount
// asking for more raises the record.
func TestTheWatchWordIsRememberedAndRestored(t *testing.T) {
	s, project, _ := store(t)
	reg := nfsserve.NewRegistry(defaultAttrs(0, 0))
	share, err := reg.Register(project, workspace.DefaultMode, workspace.ShareWords{Watch: workspace.WatchPartial})
	if err != nil {
		t.Fatal(err)
	}
	s.remember(share)

	if _, _, words, ok := newShareStore(s.path, nil).restore(share.ExportPath); !ok || words.Watch != workspace.WatchPartial {
		t.Fatalf("restored %v, %v; want watch=partial", words, ok)
	}
	if _, err := reg.Register(project, workspace.DefaultMode, workspace.ShareWords{Watch: workspace.WatchCoarse}); err != nil {
		t.Fatal(err)
	}
	s.remember(share)
	if _, _, words, _ := newShareStore(s.path, nil).restore(share.ExportPath); words.Watch != workspace.WatchCoarse {
		t.Errorf("after remembering coarse, restored %v", words)
	}
}

// A record from before the word existed restores with the session's default,
// which is what the share would have had.
func TestARecordWithoutAWatchTakesTheSessionDefault(t *testing.T) {
	st, project, _ := store(t)
	export := workspace.ExportPathForID(workspace.LegacyShareID(project))
	machine, account := thisMachine()
	data, err := json.Marshal(map[string]any{
		"version": shareFileVersion, "machine": machine, "user": account,
		"shares": []map[string]any{{"export": export, "path": project, "lastUsed": time.Now().Format(time.RFC3339)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(st.path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	s := hostingSession(t, workspace.WatchCoarse, newShareStore(st.path, nil))
	_, _, words, ok := s.restoreShare(export)
	if !ok || words.Watch != workspace.WatchCoarse {
		t.Errorf("restoreShare = %v, %v; want the session's coarse", words, ok)
	}
}

// A share restored as watching starts the watcher at once and hands it the
// share: the mount that brought it back registered nothing, so nothing else
// would, and the container started against it would be served unwatched.
func TestARestoredWatchingShareStartsTheWatcher(t *testing.T) {
	st, project, _ := store(t)
	share := shareWith(t, project, workspace.DefaultMode, workspace.ShareWords{Watch: workspace.WatchPartial})
	st.remember(share)
	export := share.ExportPath

	s := hostingSession(t, workspace.WatchOff, newShareStore(st.path, nil))
	if s.watcher() != nil {
		t.Fatal("a session that watches nothing already has a watcher")
	}
	if _, _, ok := s.registry.LookupOrRestore(export); !ok {
		t.Fatal("not restored")
	}
	w := s.watcher()
	if w == nil {
		t.Fatal("restoring a watching share did not start the watcher")
	}
	deadline := time.Now().Add(5 * time.Second)
	for w.Stats().Watched == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the restored share was never handed to the watcher")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A share that does not watch leaves a session with no watcher alone.
func TestARestoredShareThatDoesNotWatchStartsNothing(t *testing.T) {
	st, project, _ := store(t)
	share := shareWith(t, project, workspace.DefaultMode, workspace.ShareWords{Watch: workspace.WatchOff})
	st.remember(share)
	export := share.ExportPath

	s := hostingSession(t, workspace.WatchOff, newShareStore(st.path, nil))
	if _, _, ok := s.registry.LookupOrRestore(export); !ok {
		t.Fatal("not restored")
	}
	if s.watcher() != nil {
		t.Error("a share that does not watch started a watcher")
	}
}

// The watcher is not started for a session that is closing, so a mount racing
// Close cannot leave one running.
func TestNoWatcherStartsAfterClose(t *testing.T) {
	s := hostingSession(t, workspace.WatchOff, nil)
	s.closeWatcher()
	if err := s.startWatcher(); err == nil {
		t.Error("a watcher started after the session closed")
	}
}

// A mount of a variant already shared raises its watch and never lowers it, and
// the watcher is told each time, which is how a raised level reaches an export
// the watcher already holds (ADR 0042).
func TestTheRegistrarRaisesTheWatchAndNeverLowersIt(t *testing.T) {
	dir := t.TempDir()
	told := 0
	reg := shareRegistrar{registry: nfsserve.NewRegistry(defaultAttrs(0, 0)), changed: func() { told++ }}

	var export string
	for _, watch := range []workspace.Watch{workspace.WatchOff, workspace.WatchPartial, workspace.WatchOff, workspace.WatchCoarse, workspace.WatchPartial} {
		got, _, err := reg.Share(dir, mountOf(workspace.ShareWords{Watch: watch}))
		if err != nil {
			t.Fatalf("watch=%s: %v", watch, err)
		}
		if export != "" && got != export {
			t.Fatalf("watch=%s made another share: %s, then %s", watch, export, got)
		}
		export = got
	}
	share, _, _ := reg.registry.Lookup(export)
	if got := share.Words().Watch; got != workspace.WatchCoarse {
		t.Errorf("watch = %q after off, partial, off, coarse, partial; want coarse", got)
	}
	if told != 5 {
		t.Errorf("the watcher was told %d times, want once per mount", told)
	}
}

// A mount raising a share's watch on a session whose watcher is already running
// reaches the watcher, not just the registry.
func TestARaisedWatchReachesTheRunningWatcher(t *testing.T) {
	dir := t.TempDir()
	s := hostingSession(t, workspace.WatchOff, nil)
	if err := s.startWatcher(); err != nil {
		t.Fatal(err)
	}
	reg := shareRegistrar{registry: s.registry, changed: s.syncWatch}

	if _, _, err := reg.Share(dir, mountOf(workspace.ShareWords{Watch: workspace.WatchOff})); err != nil {
		t.Fatal(err)
	}
	if got := s.watcher().Stats().Watched; got != 0 {
		t.Fatalf("an unwatched share has %d watches", got)
	}
	if _, _, err := reg.Share(dir, mountOf(workspace.ShareWords{Watch: workspace.WatchPartial})); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for s.watcher().Stats().Watched == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the raised watch never reached the watcher")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Shares reach the watcher in their own mode.
func TestSharesOfCarriesEachSharesMode(t *testing.T) {
	registry := nfsserve.NewRegistry(defaultAttrs(0, 0))
	for dir, watch := range map[string]workspace.Watch{
		t.TempDir(): workspace.WatchCoarse, t.TempDir(): workspace.WatchPartial, t.TempDir(): workspace.WatchOff,
	} {
		if _, err := registry.Register(dir, workspace.DefaultMode, workspace.ShareWords{Watch: watch}); err != nil {
			t.Fatal(err)
		}
	}
	got := map[workspace.Watch]int{}
	for _, share := range sharesOf(registry) {
		got[share.Mode]++
	}
	if got[workspace.WatchCoarse] != 1 || got[workspace.WatchPartial] != 1 || got[workspace.WatchOff] != 1 {
		t.Errorf("modes = %v, want one of each", got)
	}
}
