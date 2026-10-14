package session

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/lhns/remote-docker/core-client/nfsserve"
	"github.com/lhns/remote-docker/core/workspace"
)

var alice1000 = workspace.ShareWords{Owner: workspace.Owner{UID: 1000, GID: 2000, Set: true}}

// A share's words survive a restart, and a later container naming other
// words rewrites the record.
func TestTheWordsAreRememberedAndRestored(t *testing.T) {
	s, project, export := store(t)
	s.remember(export, project, alice1000)

	_, words, ok := newShareStore(s.path, nil).restore(export)
	if !ok || words != alice1000 {
		t.Fatalf("restored %v, %v; want the owner it was remembered with", words, ok)
	}

	other := workspace.ShareWords{Owner: workspace.Owner{UID: 5, GID: 5, Set: true}}
	s.remember(export, project, other)
	if _, words, _ := newShareStore(s.path, nil).restore(export); words != other {
		t.Errorf("after remembering other words, restored %v", words)
	}
}

// A record written before the words existed has no owner field. It still
// loads at the same version (a bump would make every restore fail after an
// upgrade, ADR 0027) and restores with the words unset, which the registry
// fills from the session's default.
func TestARecordWithoutWordsStillRestores(t *testing.T) {
	s, project, export := store(t)
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

	path, words, ok := newShareStore(s.path, nil).restore(export)
	if !ok || path != project {
		t.Fatalf("restore = %q, %v", path, ok)
	}
	if words.Owner.Set {
		t.Errorf("an old record restored with owner %v, want it unset", words.Owner)
	}
}

// What the registrar hands the workspace's volume is only as good as its
// refusals. A directory already shared keeps its words, and a mount asking
// for others is told so before anything is created.
func TestTheRegistrarRefusesOtherWords(t *testing.T) {
	dir := t.TempDir()
	reg := shareRegistrar{registry: nfsserve.NewRegistry(defaultAttrs(0, 0))}

	if _, _, err := reg.Share(dir, alice1000); err != nil {
		t.Fatalf("first Share: %v", err)
	}
	// The same words are the same share.
	if _, _, err := reg.Share(dir, alice1000); err != nil {
		t.Errorf("same words refused: %v", err)
	}

	_, _, err := reg.Share(dir, workspace.ShareWords{Owner: workspace.Owner{UID: 0, GID: 0, Set: true}})
	if err == nil {
		t.Fatal("a different owner for a registered share was accepted")
	}
	for _, want := range []string{"owner=1000:2000", "owner=0:0", "\n  fix: ", "docker remote restart"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to contain %q", err, want)
		}
	}
}

// A restored share is checked like any other: the owner it comes back with is
// the one a new container must agree to.
func TestARestoredShareHoldsItsOwner(t *testing.T) {
	s, project, export := store(t)
	s.remember(export, project, alice1000)

	next := newShareStore(s.path, nil)
	registry := nfsserve.NewRegistry(defaultAttrs(0, 0))
	registry.Restore = next.restore
	if _, _, ok := registry.LookupOrRestore(export); !ok {
		t.Fatal("not restored")
	}

	reg := shareRegistrar{registry: registry, shares: next}
	if _, _, err := reg.Share(project, workspace.ShareWords{Owner: workspace.Owner{Set: true}}); err == nil {
		t.Error("a mount disagreeing with the restored owner was accepted")
	}
}
