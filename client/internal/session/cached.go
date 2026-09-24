package session

// What a delegated share's cache was filled with, remembered across sessions.
// A fill cannot carry a deletion, so this record is what lets a file deleted
// while nothing ran be removed, without touching a container's own files
// (ADR 0044).

import (
	"encoding/json"
	"log/slog"
	"sort"
	"sync"
)

// cachedFile is the fill record on disk, bound to its writer (boundRecord).
type cachedFile struct {
	boundRecord
	Shares map[string][]string `json:"shares"` // export -> paths, share-relative
}

const cachedFileVersion = 1

// cachedStore is the record of what each share's fill sent.
type cachedStore struct {
	path string
	log  *slog.Logger

	mu     sync.Mutex
	shares map[string][]string

	// saving: see shareStore.saving.
	saving sync.Mutex
}

// newCachedStore loads the record. Unreadable is empty, which only means
// nothing is removed from the cache.
func newCachedStore(path string, log *slog.Logger) *cachedStore {
	s := &cachedStore{path: path, log: log, shares: map[string][]string{}}

	var file cachedFile
	if !readBound(path, "what a cache was filled with", cachedFileVersion, log, &file) {
		return s
	}
	for export, paths := range file.Shares {
		s.shares[export] = paths
	}
	return s
}

// Filled is what the last fill of a share sent, and whether anything is known.
func (s *cachedStore) Filled(export string) ([]string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	paths, ok := s.shares[export]
	return paths, ok
}

// Record replaces what is known about a share and writes the file.
func (s *cachedStore) Record(export string, paths []string) {
	s.saving.Lock()
	defer s.saving.Unlock()

	s.mu.Lock()
	sort.Strings(paths)
	s.shares[export] = paths
	file := cachedFile{boundRecord: bindRecord(cachedFileVersion), Shares: map[string][]string{}}
	for e, p := range s.shares {
		file.Shares[e] = p
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
