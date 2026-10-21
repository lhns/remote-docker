package session

// What a delegated share's cache was filled with, remembered across sessions.
// A fill cannot carry a deletion, so this record is what lets a file deleted
// while nothing ran be removed, without touching a container's own files; and
// the size and time each file had when it was sent are what let write-back tell
// a file the container modified from one this machine made, after a restart
// (ADR 0027, ADR 0044).

import (
	"encoding/json"
	"log/slog"
	"maps"
	"sort"
	"sync"
	"time"

	"github.com/lhns/remote-docker/dircache"
)

// cachedFile is the fill record on disk, bound to its writer (boundRecord).
type cachedFile struct {
	boundRecord
	Shares map[string][]string `json:"shares"` // export -> paths, share-relative

	// Baselines is what each of those paths looked like when it was sent. It
	// sits beside Shares and not in it, so the file keeps its version: a client
	// before it reads the paths and ignores this, and one reading a file from
	// before it finds no baselines and carries back only what it never sent.
	Baselines map[string]map[string]cachedBaseline `json:"baselines,omitempty"`
}

// cachedBaseline is dircache.Baseline on disk: size and modification time in
// Unix nanoseconds.
type cachedBaseline struct {
	Size  int64 `json:"s"`
	MTime int64 `json:"m"`
}

const cachedFileVersion = 1

// cachedStore is the record of what each share's fill sent.
type cachedStore struct {
	path string
	log  *slog.Logger

	mu     sync.Mutex
	shares map[string]map[string]dircache.Baseline

	// saving: see shareStore.saving.
	saving sync.Mutex
}

// newCachedStore loads the record. Unreadable is empty, which only means
// nothing is removed from the cache.
func newCachedStore(path string, log *slog.Logger) *cachedStore {
	s := &cachedStore{path: path, log: log, shares: map[string]map[string]dircache.Baseline{}}

	var file cachedFile
	if !readBound(path, "what a cache was filled with", cachedFileVersion, log, &file) {
		return s
	}
	for export, paths := range file.Shares {
		filled := make(map[string]dircache.Baseline, len(paths))
		for _, p := range paths {
			filled[p] = dircache.Baseline{}
		}
		for p, b := range file.Baselines[export] {
			filled[p] = dircache.Baseline{Size: b.Size, ModTime: time.Unix(0, b.MTime)}
		}
		s.shares[export] = filled
	}
	return s
}

// Filled is what the last fill of a share sent, and whether anything is known.
func (s *cachedStore) Filled(export string) (map[string]dircache.Baseline, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	filled, ok := s.shares[export]
	return maps.Clone(filled), ok
}

// Record replaces what is known about a share and writes the file.
func (s *cachedStore) Record(export string, filled map[string]dircache.Baseline) {
	s.saving.Lock()
	defer s.saving.Unlock()

	s.mu.Lock()
	s.shares[export] = filled
	file := cachedFile{
		boundRecord: bindRecord(cachedFileVersion),
		Shares:      map[string][]string{},
		Baselines:   map[string]map[string]cachedBaseline{},
	}
	for e, f := range s.shares {
		paths := make([]string, 0, len(f))
		for p, b := range f {
			paths = append(paths, p)
			if !b.ModTime.IsZero() {
				if file.Baselines[e] == nil {
					file.Baselines[e] = map[string]cachedBaseline{}
				}
				file.Baselines[e][p] = cachedBaseline{Size: b.Size, MTime: b.ModTime.UnixNano()}
			}
		}
		sort.Strings(paths)
		file.Shares[e] = paths
	}
	s.mu.Unlock()

	data, err := json.Marshal(file)
	if err != nil {
		return
	}
	// Atomic: a half-written record would remove the wrong files.
	if err := writeRecord(s.path, data, 0o600); err != nil {
		s.warn("could not keep a record of what a cache holds", err)
	}
}

func (s *cachedStore) warn(msg string, err error) {
	if s.log != nil {
		s.log.Warn(msg, "path", s.path, "err", err)
	}
}
