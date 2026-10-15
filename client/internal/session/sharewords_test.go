package session

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/lhns/remote-docker/core-client/nfsserve"
	"github.com/lhns/remote-docker/core/workspace"
)

var alice1000 = workspace.ShareWords{Owner: workspace.Owner{UID: 1000, GID: 2000, Set: true}}

// A share's words survive a restart, and a later mount asking for more raises
// them in the record.
func TestTheWordsAreRememberedAndRestored(t *testing.T) {
	s, project, _ := store(t)
	reg := nfsserve.NewRegistry(defaultAttrs(0, 0))
	share, err := reg.Register(project, workspace.DefaultMode, alice1000)
	if err != nil {
		t.Fatal(err)
	}
	s.remember(share)

	_, _, words, ok := newShareStore(s.path, nil).restore(share.ExportPath)
	want := alice1000
	want.Watch, want.Prefetch = workspace.WatchOff, workspace.PrefetchOff
	if !ok || words != want {
		t.Fatalf("restored %v, %v; want the owner it was remembered with", words, ok)
	}

	raised := alice1000
	raised.Watch = workspace.WatchCoarse
	if _, err := reg.Register(project, workspace.DefaultMode, raised); err != nil {
		t.Fatal(err)
	}
	s.remember(share)
	if _, _, words, _ := newShareStore(s.path, nil).restore(share.ExportPath); words.Watch != workspace.WatchCoarse {
		t.Errorf("after the share was raised, restored %v", words)
	}
}

// A record written before the words existed has no owner field. It still
// loads at the same version (a bump would make every restore fail after an
// upgrade, ADR 0027) and restores with the words unset, which the registry
// fills from the session's default.
func TestARecordWithoutWordsStillRestores(t *testing.T) {
	s, project, _ := store(t)
	export := workspace.ExportPathForID(workspace.LegacyShareID(project))
	old := map[string]any{
		"version": shareFileVersion,
		"shares":  []map[string]any{{"export": export, "path": project, "lastUsed": "2099-01-01T00:00:00Z"}},
	}
	machine, account := thisMachine()
	old["machine"], old["user"] = machine, account
	data, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	path, mode, words, ok := newShareStore(s.path, nil).restore(export)
	if !ok || path != project {
		t.Fatalf("restore = %q, %v", path, ok)
	}
	if mode != workspace.ModeUnset {
		t.Errorf("an old record restored with mode %v, want it unknown", mode)
	}
	if words.Owner.Set {
		t.Errorf("an old record restored with owner %v, want it unset", words.Owner)
	}
}

// mountOf is what the rewriter hands the registrar: every word concrete.
func mountOf(w workspace.ShareWords) workspace.MountWords {
	if !w.Owner.Set {
		w.Owner = workspace.Owner{Set: true}
	}
	if w.Watch == workspace.WatchUnset {
		w.Watch = workspace.WatchOff
	}
	if w.Prefetch == workspace.PrefetchUnset {
		w.Prefetch = workspace.PrefetchOff
	}
	return workspace.MountWords{Mode: workspace.DefaultMode, ShareWords: w}
}

// A mount of another read, write or owner is a share of its own beside the
// first, with its own export, and nothing is refused (ADR 0042).
func TestTheRegistrarGivesEachVariantItsOwnShare(t *testing.T) {
	dir := t.TempDir()
	reg := shareRegistrar{registry: nfsserve.NewRegistry(defaultAttrs(0, 0))}

	direct := mountOf(workspace.ShareWords{})
	cached := workspace.MountWords{Mode: workspace.Mode{Read: workspace.ReadCached, Write: workspace.WriteThrough}, ShareWords: direct.ShareWords}
	back := workspace.MountWords{Mode: workspace.Mode{Read: workspace.ReadDirect, Write: workspace.WriteBack}, ShareWords: direct.ShareWords}

	exports := map[string]bool{}
	for name, mount := range map[string]workspace.MountWords{
		"direct": direct, "cached": cached, "back": back, "owned": mountOf(alice1000),
	} {
		export, _, err := reg.Share(dir, mount)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		exports[export] = true
	}
	if len(exports) != 4 {
		t.Errorf("four variants share %d exports: %v", len(exports), exports)
	}

	// The same variant again is the same share.
	again, _, err := reg.Share(dir, direct)
	if err != nil {
		t.Fatal(err)
	}
	if !exports[again] || len(reg.registry.Shares()) != 4 {
		t.Errorf("the same variant again gave %s, with %d shares", again, len(reg.registry.Shares()))
	}
}

// A share restored from the record is what a mount of the same variant finds,
// and a mount of another owner is not given it.
func TestARestoredShareIsFoundByItsVariantOnly(t *testing.T) {
	s, project, _ := store(t)
	share := shareWith(t, project, workspace.DefaultMode, alice1000)
	s.remember(share)

	next := newShareStore(s.path, nil)
	registry := nfsserve.NewRegistry(defaultAttrs(0, 0))
	registry.Restore = next.restore
	if _, _, ok := registry.LookupOrRestore(share.ExportPath); !ok {
		t.Fatal("not restored")
	}

	reg := shareRegistrar{registry: registry, shares: next}
	same, _, err := reg.Share(project, mountOf(alice1000))
	if err != nil || same != share.ExportPath {
		t.Errorf("a mount of the restored variant got %q, %v; want %q", same, err, share.ExportPath)
	}
	other, _, err := reg.Share(project, mountOf(workspace.ShareWords{Owner: workspace.Owner{Set: true}}))
	if err != nil || other == share.ExportPath {
		t.Errorf("a mount of another owner got %q, %v; want a share of its own", other, err)
	}
}

// A share restored from a 0.11 record keeps serving the containers mounting it,
// and a new mount of the directory does not land on it: that mount gets an
// export, and so a volume, of its own.
func TestANewMountIsNotGivenALegacyShare(t *testing.T) {
	s, project, _ := store(t)
	legacy := workspace.ExportPathForID(workspace.LegacyShareID(project))
	s.records[legacy] = shareRecord{Export: legacy, Path: project, LastUsed: time.Now()}
	s.save()

	next := newShareStore(s.path, nil)
	registry := nfsserve.NewRegistry(defaultAttrs(0, 0))
	registry.Restore = next.restore
	if _, _, ok := registry.LookupOrRestore(legacy); !ok {
		t.Fatal("the legacy record was not restored")
	}

	reg := shareRegistrar{registry: registry, shares: next}
	export, _, err := reg.Share(project, mountOf(workspace.ShareWords{}))
	if err != nil {
		t.Fatal(err)
	}
	if export == legacy {
		t.Error("a new mount was given the share restored from the old record")
	}
	if _, _, ok := registry.Lookup(legacy); !ok {
		t.Error("the restored share stopped resolving once a new mount arrived")
	}
}
