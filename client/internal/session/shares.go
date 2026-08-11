package session

// What this workspace has been asked to export, remembered across sessions.
//
// The registry is per process and a VOLUME outlives one. `docker compose up -d`
// on containers that already exist only starts them, so no /containers/create
// arrives, so nothing registers the share, while dockerd still mounts the
// rd-<id> volume created last time and is answered "no such file or directory".
// Recreating the containers was the only way back.
//
// THE FILE IS A CAPABILITY LIST, NOT A LOOKUP TABLE. The workspace names an id
// and this machine chooses among things it wrote down itself; the far side
// never supplies a path, and ShareID cannot be inverted, so an id nobody
// recorded resolves to nothing. Every entry is checked again before it is
// believed, because a file on disk is not evidence: see usable.

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/user"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lhns/remote-docker/client/internal/config"
	"github.com/lhns/remote-docker/pkg/workspace"
)

// shareRecord is one directory this workspace asked to export.
type shareRecord struct {
	Export   string    `json:"export"`
	Path     string    `json:"path"`
	LastUsed time.Time `json:"lastUsed"`
}

// shareFile is bound to the machine and the account that wrote it.
//
// A configuration directory is a thing people sync between machines. The same
// file elsewhere names paths that either do not exist, which is harmless, or
// exist and are a different directory, which is not. Refused wholesale rather
// than entry by entry, because a partial match is exactly the case most likely
// to be the wrong directory with the right spelling.
type shareFile struct {
	Version int           `json:"version"`
	Machine string        `json:"machine"`
	User    string        `json:"user"`
	Shares  []shareRecord `json:"shares"`
}

const shareFileVersion = 1

// shareUnused is how long a record survives without being wanted. Long enough
// to cover a project somebody comes back to, short enough that the file does
// not become a history of everything ever mounted.
const shareUnused = 30 * 24 * time.Hour

// shareStore is the record, and the only thing that may answer a mount for an
// export this session has not registered.
type shareStore struct {
	path string
	log  *slog.Logger

	mu      sync.Mutex
	records map[string]shareRecord // keyed by export path
}

// newShareStore loads the record for a workspace, dropping what no longer
// holds.
//
// A store that cannot be read is an EMPTY store rather than an error. This
// exists to make a container start that would otherwise fail, and refusing to
// run because a record file is unreadable would trade one failure for a worse
// one.
func newShareStore(path string, log *slog.Logger) *shareStore {
	s := &shareStore{path: path, log: log, records: map[string]shareRecord{}}

	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) && log != nil {
			log.Debug("no record of what this workspace exports", "path", path, "err", err)
		}
		return s
	}

	var file shareFile
	if err := json.Unmarshal(data, &file); err != nil || file.Version != shareFileVersion {
		return s
	}
	if machine, account := thisMachine(); file.Machine != machine || file.User != account {
		if log != nil {
			log.Warn("ignoring a share record written elsewhere",
				"path", path, "wrote", file.Machine+"/"+file.User)
		}
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
// export.
//
// The id is RECOMPUTED from the path, which is what makes the file
// self-checking: a hand-edited or corrupted record cannot make /m/<id> resolve
// somewhere else, because the entry would have to carry the digest of that
// somewhere else, and only this machine can produce one.
//
// What it cannot check is worth saying rather than leaving implied. A directory
// deleted and recreated as something else at the same path is indistinguishable
// without recording an inode, which is not portable; and a 64-bit id collision
// would resolve to the wrong directory, which ADR 0007 already covers by saying
// the digest is an identity and not a security boundary.
func (s *shareStore) usable(rec shareRecord) bool {
	if rec.Export == "" || rec.Path == "" {
		return false
	}
	// /cwd is registered by the session itself, from the directory the command
	// actually ran in. Remembering it would mean exporting a working directory
	// somebody has since left.
	if !strings.HasPrefix(rec.Export, workspace.ExportMountPrefix) {
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
	if s == nil || !strings.HasPrefix(exportPath, workspace.ExportMountPrefix) {
		return
	}

	s.mu.Lock()
	rec, known := s.records[exportPath]
	// Written when something changed or when the timestamp has gone stale
	// enough to matter, so an ordinary session does not rewrite the file on
	// every container it creates.
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
		// Logged, because a share coming back without a container having asked
		// for it in this session is worth being able to see afterwards.
		s.log.Info("restoring an export the workspace still has a volume for",
			"export", exportPath, "path", rec.Path)
	}
	s.remember(exportPath, rec.Path)
	return rec.Path, true
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
	s.mu.Lock()
	file := shareFile{Version: shareFileVersion, Shares: make([]shareRecord, 0, len(s.records))}
	file.Machine, file.User = thisMachine()
	for _, rec := range s.records {
		file.Shares = append(file.Shares, rec)
	}
	s.mu.Unlock()

	// Sorted, so a run that changed nothing about the set does not churn the
	// file.
	sort.Slice(file.Shares, func(i, j int) bool { return file.Shares[i].Export < file.Shares[j].Export })

	if err := writeShares(s.path, file); err != nil && s.log != nil {
		s.log.Warn("could not record what this workspace exports", "path", s.path, "err", err)
	}
}

// writeShares replaces the file atomically, so a reader never sees half of one.
//
// Through config.WriteAtomic rather than its own dance, which is not only
// shorter: that one retries the rename, because a rename can fail with a
// sharing violation while a reader has the file open, and this file is read at
// every session start and written on every share. 0o600 because it names
// directories on this machine, which is not something to hand to every account
// on it.
func writeShares(path string, file shareFile) error {
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	return config.WriteAtomic(path, append(data, '\n'), 0o600)
}

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
