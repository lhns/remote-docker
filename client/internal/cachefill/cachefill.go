// Package cachefill decides what goes into a delegated share's cache, and in
// what order (ADR 0044).
//
// The cache is allowed to be incomplete at every moment: a file it does not
// hold is served from the live export underneath, correctly, just slowly. That
// single property is what this package rests on, and it turns every hard
// question into a cheap one -- a budget that runs out, a walk that is still
// running, a file skipped for being excluded, are all the same state as a file
// the fill has not reached yet.
//
// So nothing here can fail in a way that makes a share wrong. It can only make
// one slower.
package cachefill

import (
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
)

// Budget bounds what a share will copy across.
//
// Not a refusal: over the ceiling the rest of the tree is simply served live.
// "The budget ran out" and "the fill has not got there yet" are the same state,
// so there is no size at which a delegated share stops working.
type Budget struct {
	// Files is how many entries may be cached. Zero means DefaultFiles.
	Files int

	// Bytes is how much may be copied. Zero means DefaultBytes.
	Bytes int64
}

// Chosen so an ordinary source tree fits entirely and a repository carrying
// build output or media does not silently ship gigabytes.
//
// The file count matters more than the byte count, because the win is a round
// trip saved per file: 20,000 files is a large project, and 2 GiB is far more
// than the source of one.
const (
	DefaultFiles = 20000
	DefaultBytes = 2 << 30
)

func (b Budget) files() int {
	if b.Files <= 0 {
		return DefaultFiles
	}
	return b.Files
}

func (b Budget) bytes() int64 {
	if b.Bytes <= 0 {
		return DefaultBytes
	}
	return b.Bytes
}

// Entry is one file the fill will send.
type Entry struct {
	// Path is relative to the share root, slash-separated, which is how it is
	// named in the tar and how the workspace resolves it.
	Path string

	Size int64
}

// Stats is what the walk found, for reporting a share as partly cached rather
// than as failed.
type Stats struct {
	// Files and Bytes are what will be cached.
	Files int
	Bytes int64

	// TotalFiles and TotalBytes are what the tree holds, excludes aside, so
	// the two can be reported as a fraction.
	TotalFiles int
	TotalBytes int64

	// Excluded is how many entries the exclude list skipped. Reported because
	// a cache that mysteriously omits .git is worth being able to explain.
	Excluded int
}

// Complete reports whether everything the walk saw will be cached.
//
// What write-back later depends on: a cache that is complete can be compared
// with the tree, and one that is not cannot say whether a missing file was
// deleted by the container or simply never sent.
func (s Stats) Complete() bool { return s.Files == s.TotalFiles }

// Plan walks a share and decides what to cache, in the order to send it.
//
// SMALLEST FIRST, which is the whole of the ordering policy. The win is a round
// trip saved per file, so a thousand small files are worth far more than one
// large one that costs the same bandwidth and saves a single round trip. A
// repository of source and build assets therefore caches its source and serves
// its assets live, with nobody having to configure that.
//
// Errors are skipped rather than returned. A directory that cannot be read is a
// part of the tree that stays live, which is the same outcome as a budget that
// ran out, and refusing the whole share over one unreadable path would be worse
// than the thing it protects against.
func Plan(root string, excludes []string, budget Budget) ([]Entry, Stats) {
	var (
		found []Entry
		stats Stats
	)
	skip := excluded(excludes)

	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if p == root {
			return nil
		}
		if skip[d.Name()] {
			stats.Excluded++
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() || !d.Type().IsRegular() {
			// Directories arrive with the files inside them, and what a tar
			// cannot carry to another machine -- sockets, devices, pipes -- is
			// not cached at all. The live export answers for those.
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}

		stats.TotalFiles++
		stats.TotalBytes += info.Size()
		found = append(found, Entry{Path: filepath.ToSlash(rel), Size: info.Size()})
		return nil
	})

	sort.SliceStable(found, func(i, j int) bool { return found[i].Size < found[j].Size })

	kept := found[:0]
	for _, e := range found {
		if len(kept) >= budget.files() || stats.Bytes+e.Size > budget.bytes() {
			// Stop at the ceiling rather than skipping this one and trying the
			// next: the list is sorted, so everything after it is larger, and
			// carrying on would spend the whole budget looking for something
			// small enough to fit.
			break
		}
		kept = append(kept, e)
		stats.Files++
		stats.Bytes += e.Size
	}
	return kept, stats
}

// excluded is the set of directory names never cached.
//
// The same list the watcher uses, and that is a rule rather than a convenience:
// the cache may only hold what the watcher can invalidate, or a file changed
// here would stay stale in the container until something else removed it.
func excluded(names []string) map[string]bool {
	set := make(map[string]bool, len(names))
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			set[n] = true
		}
	}
	return set
}

// Batches groups entries into sends of at most maxBytes.
//
// Batched because the channel frames a payload by length: the workspace has to
// be told how many bytes follow before they are sent, so a batch is built in
// memory and its size is what bounds that. One file larger than the batch size
// still goes on its own -- a batch is a limit on grouping, not on a file.
func Batches(entries []Entry, maxBytes int64) [][]Entry {
	var (
		out   [][]Entry
		batch []Entry
		size  int64
	)
	for _, e := range entries {
		if len(batch) > 0 && size+e.Size > maxBytes {
			out = append(out, batch)
			batch, size = nil, 0
		}
		batch = append(batch, e)
		size += e.Size
	}
	if len(batch) > 0 {
		out = append(out, batch)
	}
	return out
}

// DefaultBatchBytes is how much one send carries.
//
// 16 MiB: large enough that a source tree is a handful of sends rather than
// thousands, small enough to hold in memory while it is framed, and small
// enough that a share becomes useful in pieces rather than all at once.
const DefaultBatchBytes = 16 << 20
