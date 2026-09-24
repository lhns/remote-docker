package session

// What this workspace has been asked to export, remembered across sessions,
// so a volume that outlives the process still mounts when `compose up -d`
// starts containers without creating them (ADR 0027).
//
// A CAPABILITY LIST, NOT A LOOKUP TABLE: the workspace names an id, never a
// path, and every entry is re-checked before it is believed (usable).

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/user"
	"sort"
	"sync"
	"time"

	"github.com/lhns/remote-docker/client/internal/config"
	"github.com/lhns/remote-docker/core/workspace"
)

// shareRecord is one directory this workspace asked to export.
type shareRecord struct {
	Export   string    `json:"export"`
	Path     string    `json:"path"`
	LastUsed time.Time `json:"lastUsed"`
}

// shareFile is the share record on disk.
type shareFile struct {
	boundRecord
	Shares []shareRecord `json:"shares"`
}

const shareFileVersion = 1

// boundRecord binds a record to the machine and account that wrote it. A
// synced config directory could otherwise name a different directory with the
// right spelling, so a mismatch refuses the whole file.
type boundRecord struct {
	Version int    `json:"version"`
	Machine string `json:"machine"`
	User    string `json:"user"`
}

func (b boundRecord) bound() boundRecord { return b }

// bindRecord is the header for a record this machine writes now.
func bindRecord(version int) boundRecord {
	b := boundRecord{Version: version}
	b.Machine, b.User = thisMachine()
	return b
}

// readBound reads a record into `into` and reports whether it may be
// believed: readable, at the version wanted, and written by this machine and
// account. `what` names the record in the log.
func readBound(path, what string, version int, log *slog.Logger, into interface{ bound() boundRecord }) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) && log != nil {
			log.Debug("no record of "+what, "path", path, "err", err)
		}
		return false
	}
	if err := json.Unmarshal(data, into); err != nil || into.bound().Version != version {
		return false
	}
	if machine, account := thisMachine(); into.bound().Machine != machine || into.bound().User != account {
		if log != nil {
			log.Warn("ignoring "+what+": written on another machine or by another account",
				"path", path, "wrote", into.bound().Machine+"/"+into.bound().User)
		}
		return false
	}
	return true
}

// shareUnused is how long a record survives without being wanted.
const shareUnused = 30 * 24 * time.Hour

// shareStore is the record, and the only thing that may answer a mount for an
// export this session has not registered.
type shareStore struct {
	path string
	log  *slog.Logger

	mu      sync.Mutex
	records map[string]shareRecord // keyed by export path

	// saving spans taking the copy AND writing it, so two saves cannot land
	// in the opposite order and leave the older set on disk.
	saving sync.Mutex
}

// newShareStore loads the record, dropping what no longer holds. Unreadable
// means empty, never an error.
func newShareStore(path string, log *slog.Logger) *shareStore {
	s := &shareStore{path: path, log: log, records: map[string]shareRecord{}}

	var file shareFile
	if !readBound(path, "what this workspace exports", shareFileVersion, log, &file) {
		return s
	}
	for _, rec := range file.Shares {
		if s.usable(rec) {
			s.records[rec.Export] = rec
		}
	}
	return s
}

// usable reports whether a record still describes a directory this machine may
// export. The id is RECOMPUTED from the path, so an edited record cannot make
// /m/<id> resolve anywhere else. A directory recreated at the same path is not
// detected.
func (s *shareStore) usable(rec shareRecord) bool {
	if rec.Export == "" || rec.Path == "" {
		return false
	}
	if rec.Export != workspace.ExportPathForID(workspace.ShareID(rec.Path)) {
		return false
	}
	if !rec.LastUsed.IsZero() && time.Since(rec.LastUsed) > shareUnused {
		return false
	}
	info, err := os.Stat(rec.Path)
	return err == nil && info.IsDir()
}

// remember records a share, so a container started later can still be served.
func (s *shareStore) remember(exportPath, localPath string) {
	if s == nil {
		return
	}

	s.mu.Lock()
	rec, known := s.records[exportPath]
	// Not rewritten on every container: only on change or an hour-stale stamp.
	changed := !known || rec.Path != localPath || time.Since(rec.LastUsed) > time.Hour
	if changed {
		s.records[exportPath] = shareRecord{Export: exportPath, Path: localPath, LastUsed: time.Now()}
	}
	s.mu.Unlock()

	if changed {
		s.save()
	}
}

// restore answers a mount for an export this session has not registered.
func (s *shareStore) restore(exportPath string) (string, bool) {
	if s == nil {
		return "", false
	}

	s.mu.Lock()
	rec, ok := s.records[exportPath]
	s.mu.Unlock()

	if !ok || !s.usable(rec) {
		return "", false
	}

	if s.log != nil {
		s.log.Info("restoring an export the workspace still has a volume for",
			"export", exportPath, "path", rec.Path)
	}
	s.remember(exportPath, rec.Path)
	return rec.Path, true
}

// exports names every recorded export, for matching a root handle to one.
func (s *shareStore) exports() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.records))
	for export := range s.records {
		out = append(out, export)
	}
	sort.Strings(out)
	return out
}

// forget drops records for exports the workspace no longer has a volume for.
func (s *shareStore) forget(keep map[string]bool) {
	if s == nil {
		return
	}

	s.mu.Lock()
	dropped := false
	for export := range s.records {
		if !keep[export] {
			delete(s.records, export)
			dropped = true
		}
	}
	s.mu.Unlock()

	if dropped {
		s.save()
	}
}

// save writes the record, and never fails a command over it.
func (s *shareStore) save() {
	s.saving.Lock()
	defer s.saving.Unlock()

	s.mu.Lock()
	file := shareFile{boundRecord: bindRecord(shareFileVersion), Shares: make([]shareRecord, 0, len(s.records))}
	for _, rec := range s.records {
		file.Shares = append(file.Shares, rec)
	}
	s.mu.Unlock()

	// Sorted, so an unchanged set does not churn the file.
	sort.Slice(file.Shares, func(i, j int) bool { return file.Shares[i].Export < file.Shares[j].Export })

	if err := writeShares(s.path, file); err != nil && s.log != nil {
		s.log.Warn("could not record what this workspace exports", "path", s.path, "err", err)
	}
}

// writeShares replaces the file atomically; config.WriteAtomic retries a
// rename that hits a Windows sharing violation. 0o600: it names local paths.
func writeShares(path string, file shareFile) error {
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	return writeRecord(path, append(data, '\n'), 0o600)
}

// writeRecord is how both records reach the disk; a test replaces it to
// order two saves.
var writeRecord = config.WriteAtomic

// thisMachine names the host and local account a record belongs to.
func thisMachine() (string, string) {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}
	account := "unknown"
	if u, err := user.Current(); err == nil {
		account = u.Username
	}
	return host, account
}
