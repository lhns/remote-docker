package session

// The record of what a workspace exports, which is the only thing that may
// answer a mount this session never registered.
//
// Everything here runs with no docker and no daemon.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lhns/remote-docker/pkg/workspace"
)

// store builds a record over a temporary file, with one real directory to
// export.
func store(t *testing.T) (*shareStore, string, string) {
	t.Helper()

	dir := t.TempDir()
	project := filepath.Join(dir, "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	export := workspace.ExportPathForID(workspace.ShareID(project))
	return newShareStore(filepath.Join(dir, "shares.json"), nil), project, export
}

// The failure this exists for. A container CREATED in one session is STARTED
// in another, and only creation registers a share.
func TestARememberedShareIsRestoredInAnotherSession(t *testing.T) {
	s, project, export := store(t)
	s.remember(export, project)

	// Another session over the same record: nothing registered, everything
	// remembered.
	next := newShareStore(s.path, nil)
	got, ok := next.restore(export)
	if !ok {
		t.Fatal("a share this workspace has a volume for was not restored")
	}
	if got != project {
		t.Errorf("restored %q, want %q", got, project)
	}
}

// An id nobody wrote down resolves to nothing. ShareID cannot be inverted, so
// this is the whole of what stops the far side naming a directory.
func TestAnUnknownExportRestoresNothing(t *testing.T) {
	s, _, _ := store(t)

	if _, ok := s.restore("/m/0123456789abcdef"); ok {
		t.Error("an export nobody recorded was restored")
	}
	if _, ok := s.restore(workspace.ExportCWD); ok {
		t.Error("the working directory was restored from a record")
	}
}

// The record is checked again on the way out, not trusted because it is on
// disk. An entry whose id does not recompute from its path is a corrupted or
// edited file, and believing it would resolve /m/<id> to a directory this
// machine never offered.
func TestATamperedRecordIsRefused(t *testing.T) {
	s, project, export := store(t)
	s.remember(export, project)

	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.MkdirAll(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}

	var file shareFile
	data, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	file.Shares[0].Path = elsewhere
	if err := writeShares(s.path, file); err != nil {
		t.Fatal(err)
	}

	if _, ok := newShareStore(s.path, nil).restore(export); ok {
		t.Error("an export was resolved to a directory whose id does not match it")
	}
}

// A path that has gone is dropped rather than offered.
func TestAMissingDirectoryIsDropped(t *testing.T) {
	s, project, export := store(t)
	s.remember(export, project)

	if err := os.RemoveAll(project); err != nil {
		t.Fatal(err)
	}
	if _, ok := newShareStore(s.path, nil).restore(export); ok {
		t.Error("a directory that no longer exists was restored")
	}
}

// A configuration directory is a thing people sync. The same file on another
// machine names paths that either do not exist or are a different directory
// with the same spelling, and the second is the dangerous one.
func TestARecordFromAnotherMachineIsIgnored(t *testing.T) {
	s, project, export := store(t)
	s.remember(export, project)

	var file shareFile
	data, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	file.Machine = "somebody-elses-laptop"
	if err := writeShares(s.path, file); err != nil {
		t.Fatal(err)
	}

	if _, ok := newShareStore(s.path, nil).restore(export); ok {
		t.Error("a record written on another machine was used")
	}
}

// A record nobody has wanted for a month stops being offered, so the file does
// not become a history of everything ever mounted.
func TestAnOldRecordExpires(t *testing.T) {
	s, project, export := store(t)

	s.records[export] = shareRecord{
		Export:   export,
		Path:     project,
		LastUsed: time.Now().Add(-2 * shareUnused),
	}
	s.save()

	if _, ok := newShareStore(s.path, nil).restore(export); ok {
		t.Error("a record older than the expiry was still offered")
	}
}

// Forgetting follows the workspace: an export whose volume is gone can never
// be asked for again.
func TestForgetDropsWhatHasNoVolume(t *testing.T) {
	s, project, export := store(t)
	s.remember(export, project)

	s.forget(map[string]bool{"/m/0123456789abcdef": true})
	if _, ok := s.restore(export); ok {
		t.Error("a record survived the volume it belonged to")
	}
	if _, ok := newShareStore(s.path, nil).restore(export); ok {
		t.Error("the dropped record came back from the file")
	}
}

// An unreadable or unwritable record must never fail a command: this exists to
// make a container start, not to add a new way for one to fail.
func TestAnUnusableRecordIsEmptyRatherThanFatal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shares.json")
	if err := os.WriteFile(path, []byte("this is not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	s := newShareStore(path, nil)
	if len(s.records) != 0 {
		t.Errorf("a corrupt file produced %d records", len(s.records))
	}
	// And it still records, rather than staying broken forever.
	project := filepath.Join(dir, "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	export := workspace.ExportPathForID(workspace.ShareID(project))
	s.remember(export, project)

	if _, ok := newShareStore(path, nil).restore(export); !ok {
		t.Error("the record did not recover after a corrupt file")
	}
}

// A nil store is what a query session has, and it must restore nothing rather
// than panic.
func TestANilStoreRestoresNothing(t *testing.T) {
	var s *shareStore
	if _, ok := s.restore("/m/0123456789abcdef"); ok {
		t.Error("a session with no record restored something")
	}
	s.remember("/m/0123456789abcdef", "somewhere")
	s.forget(nil)
}
