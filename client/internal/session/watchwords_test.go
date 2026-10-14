package session

import (
	"encoding/json"
	"os"
	"strings"
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

// The watch word is recorded with the share, resolved, and a later container
// naming another rewrites the record.
func TestTheWatchWordIsRememberedAndRestored(t *testing.T) {
	s, project, export := store(t)
	partial := workspace.ShareWords{Watch: workspace.WatchPartial}
	s.remember(export, project, partial)

	if _, words, ok := newShareStore(s.path, nil).restore(export); !ok || words.Watch != workspace.WatchPartial {
		t.Fatalf("restored %v, %v; want watch=partial", words, ok)
	}
	s.remember(export, project, workspace.ShareWords{Watch: workspace.WatchCoarse})
	if _, words, _ := newShareStore(s.path, nil).restore(export); words.Watch != workspace.WatchCoarse {
		t.Errorf("after remembering coarse, restored %v", words)
	}
}

// A record from before the word existed restores with the session's default,
// which is what the share would have had.
func TestARecordWithoutAWatchTakesTheSessionDefault(t *testing.T) {
	st, project, export := store(t)
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
	_, words, ok := s.restoreShare(export)
	if !ok || words.Watch != workspace.WatchCoarse {
		t.Errorf("restoreShare = %v, %v; want the session's coarse", words, ok)
	}
}

// A share restored as watching starts the watcher at once and hands it the
// share: the mount that brought it back registered nothing, so nothing else
// would, and the container started against it would be served unwatched.
func TestARestoredWatchingShareStartsTheWatcher(t *testing.T) {
	st, project, export := store(t)
	st.remember(export, project, workspace.ShareWords{Watch: workspace.WatchPartial})

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
	st, project, export := store(t)
	st.remember(export, project, workspace.ShareWords{Watch: workspace.WatchOff})

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

// The registrar compares the RESOLVED watch, so a mount taking the default
// against a share that watches is refused, naming the key and the way out.
func TestTheRegistrarRefusesAnotherWatch(t *testing.T) {
	dir := t.TempDir()
	reg := shareRegistrar{registry: nfsserve.NewRegistry(defaultAttrs(0, 0))}

	if _, _, err := reg.Share(dir, workspace.ShareWords{Watch: workspace.WatchPartial}); err != nil {
		t.Fatal(err)
	}
	_, _, err := reg.Share(dir, workspace.ShareWords{Watch: workspace.WatchOff})
	if err == nil {
		t.Fatal("a different watch for a registered share was accepted")
	}
	for _, want := range []string{"watch=partial", "watch=off", "\n  fix: ", "docker remote restart"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to contain %q", err, want)
		}
	}
	if !strings.Contains(err.Error(), dir) {
		t.Errorf("err = %q, want the directory named", err)
	}
}

// Shares reach the watcher in their own mode.
func TestSharesOfCarriesEachSharesMode(t *testing.T) {
	registry := nfsserve.NewRegistry(defaultAttrs(0, 0))
	for dir, watch := range map[string]workspace.Watch{
		t.TempDir(): workspace.WatchCoarse, t.TempDir(): workspace.WatchPartial, t.TempDir(): workspace.WatchOff,
	} {
		if _, err := registry.Register(dir, workspace.ShareWords{Watch: watch}); err != nil {
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
