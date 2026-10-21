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
	"github.com/lhns/remote-docker/core-client/nfsserve"
	"github.com/lhns/remote-docker/core/workspace"
)

// shareRecord is one directory this workspace asked to export.
//
// Read, Write and Owner are the share's variant and Watch and Prefetch its
// raised words, all resolved. A record without one predates the word and takes
// the session's default, which is why the file version does not move: a bump
// would make every restore fail after an upgrade (ADR 0027). A record with no
// read and write was written before the variant was in the export id; it
// restores under its recorded export and the mode stays unknown.
type shareRecord struct {
	Export   string    `json:"export"`
	Path     string    `json:"path"`
	LastUsed time.Time `json:"lastUsed"`
	Read     string    `json:"read,omitempty"`
	Write    string    `json:"write,omitempty"`
	Owner    string    `json:"owner,omitempty"` // uid:gid
	Watch    string    `json:"watch,omitempty"`
	Prefetch string    `json:"prefetch,omitempty"`
}

// mode reads the record's variant mode. Unset when the record predates it, or
// when it does not parse.
func (r shareRecord) mode() workspace.Mode {
	if r.Read == "" || r.Write == "" {
		return workspace.ModeUnset
	}
	got, err := workspace.ParseMode("read=" + r.Read + ",write=" + r.Write)
	if err != nil {
		return workspace.ModeUnset
	}
	return got
}

// words reads the record's share words. A field that does not parse is left
// unset, so the share takes the session's default rather than being lost.
func (r shareRecord) words() workspace.ShareWords {
	var w workspace.ShareWords
	if o, err := workspace.ParseOwner(r.Owner); err == nil {
		w.Owner = o
	}
	if watch, err := workspace.ParseWatch(r.Watch); err == nil {
		w.Watch = watch
	}
	if prefetch, err := workspace.ParsePrefetch(r.Prefetch); err == nil {
		w.Prefetch = prefetch
	}
	return w
}

// recordFor is the record of one share as it stands: its variant and the words
// it has been raised to.
func recordFor(share *nfsserve.Share) shareRecord {
	words := share.Words()
	rec := shareRecord{Export: share.ExportPath, Path: share.LocalPath, LastUsed: time.Now()}
	if share.Mode != workspace.ModeUnset {
		rec.Read, rec.Write = string(share.Mode.Read), string(share.Mode.Write)
	}
	if words.Owner.Set {
		rec.Owner = words.Owner.String()
	}
	rec.Watch, rec.Prefetch = string(words.Watch), string(words.Prefetch)
	return rec
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
// export. The id is RECOMPUTED from the path and the variant
// (nfsserve.RecordedExport), so an edited record cannot make /m/<id> resolve
// anywhere else. A directory recreated at the same path is not detected.
func (s *shareStore) usable(rec shareRecord) bool {
	if rec.Export == "" || rec.Path == "" {
		return false
	}
	if rec.Export != nfsserve.RecordedExport(rec.Path, rec.mode(), rec.words().Owner) {
		return false
	}
	if !rec.LastUsed.IsZero() && time.Since(rec.LastUsed) > shareUnused {
		return false
	}
	info, err := os.Stat(rec.Path)
	return err == nil && info.IsDir()
}

// remember records a share, so a container started later can still be served.
func (s *shareStore) remember(share *nfsserve.Share) {
	if s == nil {
		return
	}

	s.mu.Lock()
	rec, known := s.records[share.ExportPath]
	// Not rewritten on every container: only on change or an hour-stale stamp.
	next := recordFor(share)
	changed := !known || rec.Path != next.Path || rec.Read != next.Read || rec.Write != next.Write ||
		rec.Owner != next.Owner || rec.Watch != next.Watch || rec.Prefetch != next.Prefetch || time.Since(rec.LastUsed) > time.Hour
	if changed {
		s.records[share.ExportPath] = next
	}
	s.mu.Unlock()

	if changed {
		s.save()
	}
}

// restore answers a mount for an export this session has not registered, with
// the record's variant: an unset mode says it predates the variant.
func (s *shareStore) restore(exportPath string) (string, workspace.Mode, workspace.ShareWords, bool) {
	if s == nil {
		return "", workspace.ModeUnset, workspace.ShareWords{}, false
	}

	s.mu.Lock()
	rec, ok := s.records[exportPath]
	s.mu.Unlock()

	if !ok || !s.usable(rec) {
		return "", workspace.ModeUnset, workspace.ShareWords{}, false
	}

	if s.log != nil {
		s.log.Info("restoring an export the workspace still has a volume for",
			"export", exportPath, "path", rec.Path)
	}
	s.touch(exportPath)
	return rec.Path, rec.mode(), rec.words(), true
}

// touch refreshes a record's stamp, so one a container still mounts is not
// forgotten for being unused.
func (s *shareStore) touch(exportPath string) {
	s.mu.Lock()
	rec, ok := s.records[exportPath]
	if ok {
		rec.LastUsed = time.Now()
		s.records[exportPath] = rec
	}
	s.mu.Unlock()
	if ok {
		s.save()
	}
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

// unionExports names the recorded exports that are unions, which are the only
// ones a cache channel has anything to say about.
func (s *shareStore) unionExports() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for export, rec := range s.records {
		if rec.mode().Union() && s.usable(rec) {
			out = append(out, export)
		}
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
