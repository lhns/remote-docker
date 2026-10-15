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

	"github.com/lhns/remote-docker/core-client/nfsserve"
	"github.com/lhns/remote-docker/core/workspace"
)

// shareWith registers dir the way a mount does, as the variant and words given,
// and returns the share.
func shareWith(t *testing.T, dir string, mode workspace.Mode, words workspace.ShareWords) *nfsserve.Share {
	t.Helper()
	share, err := nfsserve.NewRegistry(defaultAttrs(0, 0)).Register(dir, mode, words)
	if err != nil {
		t.Fatal(err)
	}
	return share
}

// shareOf is shareWith for a mount that said nothing.
func shareOf(t *testing.T, dir string) *nfsserve.Share {
	t.Helper()
	return shareWith(t, dir, workspace.DefaultMode, workspace.ShareWords{})
}

// store builds a record over a temporary file, with one real directory to
// export.
func store(t *testing.T) (*shareStore, string, string) {
	t.Helper()

	dir := t.TempDir()
	project := filepath.Join(dir, "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	export := shareOf(t, project).ExportPath
	return newShareStore(filepath.Join(dir, "shares.json"), nil), project, export
}

// The failure this exists for. A container CREATED in one session is STARTED
// in another, and only creation registers a share.
func TestARememberedShareIsRestoredInAnotherSession(t *testing.T) {
	s, project, export := store(t)
	s.remember(shareOf(t, project))

	// Another session over the same record: nothing registered, everything
	// remembered.
	next := newShareStore(s.path, nil)
	got, _, _, ok := next.restore(export)
	if !ok {
		t.Fatal("a share this workspace has a volume for was not restored")
	}
	if got != project {
		t.Errorf("restored %q, want %q", got, project)
	}
}

// An id nobody wrote down resolves to nothing. A share id cannot be inverted, so
// this is the whole of what stops the far side naming a directory.
func TestAnUnknownExportRestoresNothing(t *testing.T) {
	s, _, _ := store(t)

	if _, _, _, ok := s.restore("/m/0123456789abcdef"); ok {
		t.Error("an export nobody recorded was restored")
	}
}

// The record is checked again on the way out, not trusted because it is on
// disk. An entry whose id does not recompute from its path is a corrupted or
// edited file, and believing it would resolve /m/<id> to a directory this
// machine never offered.
func TestATamperedRecordIsRefused(t *testing.T) {
	s, project, export := store(t)
	s.remember(shareOf(t, project))

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

	if _, _, _, ok := newShareStore(s.path, nil).restore(export); ok {
		t.Error("an export was resolved to a directory whose id does not match it")
	}
}

// A path that has gone is dropped rather than offered.
func TestAMissingDirectoryIsDropped(t *testing.T) {
	s, project, export := store(t)
	s.remember(shareOf(t, project))

	if err := os.RemoveAll(project); err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok := newShareStore(s.path, nil).restore(export); ok {
		t.Error("a directory that no longer exists was restored")
	}
}

// A configuration directory is a thing people sync. The same file on another
// machine names paths that either do not exist or are a different directory
// with the same spelling, and the second is the dangerous one.
func TestARecordFromAnotherMachineIsIgnored(t *testing.T) {
	s, project, export := store(t)
	s.remember(shareOf(t, project))

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

	if _, _, _, ok := newShareStore(s.path, nil).restore(export); ok {
		t.Error("a record written on another machine was used")
	}
}

// A record nobody has wanted for a month stops being offered, so the file does
// not become a history of everything ever mounted.
func TestAnOldRecordExpires(t *testing.T) {
	s, project, export := store(t)

	rec := recordFor(shareOf(t, project))
	rec.LastUsed = time.Now().Add(-2 * shareUnused)
	s.records[export] = rec
	s.save()

	if _, _, _, ok := newShareStore(s.path, nil).restore(export); ok {
		t.Error("a record older than the expiry was still offered")
	}
}

// Forgetting follows the workspace: an export whose volume is gone can never
// be asked for again.
func TestForgetDropsWhatHasNoVolume(t *testing.T) {
	s, project, export := store(t)
	s.remember(shareOf(t, project))

	s.forget(map[string]bool{"/m/0123456789abcdef": true})
	if _, _, _, ok := s.restore(export); ok {
		t.Error("a record survived the volume it belonged to")
	}
	if _, _, _, ok := newShareStore(s.path, nil).restore(export); ok {
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
	export := shareOf(t, project).ExportPath
	s.remember(shareOf(t, project))

	if _, _, _, ok := newShareStore(path, nil).restore(export); !ok {
		t.Error("the record did not recover after a corrupt file")
	}
}

// A nil store is what a query session has, and it must restore nothing rather
// than panic.
func TestANilStoreRestoresNothing(t *testing.T) {
	var s *shareStore
	if _, _, _, ok := s.restore("/m/0123456789abcdef"); ok {
		t.Error("a session with no record restored something")
	}
	s.remember(&nfsserve.Share{})
	s.forget(nil)
}

func TestConcurrentRemembersKeepTheNewerSet(t *testing.T) {
	s, project, export := store(t)
	other := filepath.Join(filepath.Dir(project), "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	otherShare := shareOf(t, other)
	otherExport := otherShare.ExportPath
	held := overtakenWrite(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.remember(shareOf(t, project))
	}()
	<-held
	s.remember(otherShare)
	<-done

	next := newShareStore(s.path, nil)
	for _, e := range []string{export, otherExport} {
		if _, _, _, ok := next.restore(e); !ok {
			t.Errorf("%s is missing on disk after two concurrent remembers", e)
		}
	}
}

// The variant is recorded with the share, and what comes back is the mode it
// was registered with: that, with the owner, is what the export id derives from.
func TestARecordCarriesTheVariant(t *testing.T) {
	st, project, _ := store(t)
	share := shareWith(t, project, workspace.Mode{Read: workspace.ReadCached, Write: workspace.WriteBack}, alice1000)
	st.remember(share)

	path, mode, words, ok := newShareStore(st.path, nil).restore(share.ExportPath)
	if !ok || path != project {
		t.Fatalf("restore = %q, %v", path, ok)
	}
	if want := (workspace.Mode{Read: workspace.ReadCached, Write: workspace.WriteBack}); mode != want {
		t.Errorf("restored mode = %v, want %v", mode, want)
	}
	if words.Owner != alice1000.Owner {
		t.Errorf("restored owner = %v, want %v", words.Owner, alice1000.Owner)
	}
}

// A record written before the variant was in the id has no read or write and
// the export a path alone derived. It restores under that export with the mode
// unknown, so the registry keeps it out of the by-path lookup.
func TestALegacyRecordRestoresWithoutAVariant(t *testing.T) {
	st, project, _ := store(t)
	legacy := workspace.ExportPathForID(workspace.LegacyShareID(project))
	st.records[legacy] = shareRecord{Export: legacy, Path: project, LastUsed: time.Now(), Owner: "1000:2000"}
	st.save()

	path, mode, words, ok := newShareStore(st.path, nil).restore(legacy)
	if !ok || path != project {
		t.Fatalf("restore = %q, %v; want the 0.11 record to restore", path, ok)
	}
	if mode != workspace.ModeUnset {
		t.Errorf("restored mode = %v, want it unknown", mode)
	}
	if words.Owner != alice1000.Owner {
		t.Errorf("restored owner = %v", words.Owner)
	}
}

// A record whose export is not what its path and variant derive is dropped,
// whether the variant was edited or the export was borrowed from another.
func TestARecordWhoseExportDoesNotMatchItsVariantIsDropped(t *testing.T) {
	st, project, _ := store(t)
	share := shareWith(t, project, workspace.Mode{Read: workspace.ReadCached, Write: workspace.WriteBack}, workspace.ShareWords{})

	for name, edit := range map[string]func(*shareRecord){
		"another read":  func(r *shareRecord) { r.Read = string(workspace.ReadDirect) },
		"another write": func(r *shareRecord) { r.Write = string(workspace.WriteThrough) },
		"another owner": func(r *shareRecord) { r.Owner = "1000:1000" },
		"no owner":      func(r *shareRecord) { r.Owner = "" },
		"the legacy id": func(r *shareRecord) { r.Export = workspace.ExportPathForID(workspace.LegacyShareID(r.Path)) },
	} {
		t.Run(name, func(t *testing.T) {
			rec := recordFor(share)
			edit(&rec)
			next := newShareStore(filepath.Join(t.TempDir(), "shares.json"), nil)
			next.records[rec.Export] = rec
			if _, _, _, ok := next.restore(rec.Export); ok {
				t.Errorf("restored %+v", rec)
			}
		})
	}

	// And a variant record under the id a path alone derives is not a legacy
	// record: the legacy shape has no read or write.
	legacy := workspace.ExportPathForID(workspace.LegacyShareID(project))
	rec := recordFor(share)
	rec.Export = legacy
	st.records[legacy] = rec
	if _, _, _, ok := st.restore(legacy); ok {
		t.Error("a record with a variant was restored under the legacy export")
	}
}
