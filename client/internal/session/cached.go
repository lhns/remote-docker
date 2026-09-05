package session

// What a delegated share's cache was filled with, remembered across sessions.
//
// A fill only ever writes: it overwrites what changed and adds what is new, and
// has no way to notice what is GONE. So a file deleted here while nothing was
// running stays in the cache and stays visible to every container (ADR 0044).
//
// Telling that from a container's own file needs one thing the session cannot
// work out: whether the FILL put it there. That is what this records. Only the
// paths, because a change made while away is carried by the next fill anyway.

import (
	"encoding/json"
	"log/slog"
	"sort"
	"sync"

	"github.com/lhns/remote-docker/client/internal/config"
)

// cachedFile is the fill record on disk, bound to its writer exactly as the
// share record is (boundRecord): this one decides what to REMOVE from a cache.
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
}

// newCachedStore loads the record, or an empty one.
//
// Unreadable is EMPTY rather than an error, and the consequence is the one this
// whole file exists to reduce rather than a failure: without a record nothing
// is removed from the cache, so a share is stale in the way it was before this
// existed. Refusing to run would be worse.
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
	// config.WriteAtomic rather than a write and a rename of our own: a
	// half-written record is one that decides to remove the wrong files from
	// somebody's cache, and the rename needs the retry that helper carries for
	// a Windows sharing violation. The share record already goes through it.
	if err := config.WriteAtomic(s.path, data, 0o600); err != nil {
		s.warn("could not keep a record of what a cache holds", err)
	}
}

func (s *cachedStore) warn(msg string, err error) {
	if s.log != nil {
		s.log.Warn(msg, "path", s.path, "err", err)
	}
}
