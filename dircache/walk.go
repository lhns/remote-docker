package dircache

// What a share offers its cache: the walk, the budget, and the batch bounds.

import (
	"io/fs"
	"path/filepath"
	"strings"
)

// Budget bounds what a share will copy across.
//
// Not a refusal: over the ceiling the rest of the tree is simply served live,
// so there is no size at which a share stops working.
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

// Entry is one file a prefetch may send.
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

// walk hands over each entry to cache as it is found, so sending can start
// before the scan ends.
//
// The budget is NOT applied here, or directory order would decide what a share
// caches; next and finishIfWalked apply it where the candidates can be seen.
// An unreadable directory is skipped and stays live, like a spent budget.
func walk(root string, excludes []string, yield func(Entry)) Stats {
	var stats Stats
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
		// Directories arrive with the files inside them, and what a tar cannot
		// carry to another machine (sockets, devices, pipes) is not cached at
		// all. The live export answers for those.
		if d.IsDir() || !d.Type().IsRegular() {
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
		yield(Entry{Path: filepath.ToSlash(rel), Size: info.Size()})
		return nil
	})
	return stats
}

// excluded is the set of directory names never cached; see Cache.Exclude.
func excluded(names []string) map[string]bool {
	set := make(map[string]bool, len(names))
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			set[n] = true
		}
	}
	return set
}

// batchBytes is how much one send carries.
//
// 16 MiB: large enough that a source tree is a handful of sends rather than
// thousands, small enough to hold in memory while it is framed, and small
// enough that a share becomes useful in pieces rather than all at once.
const batchBytes = 16 << 20

// maxBatchFiles bounds a batch by count as well as by bytes.
//
// A batch of a hundred thousand empty files is small in bytes and slow in every
// other way: it is built in memory, framed, extracted entry by entry, and its
// failure costs all of it. Two thousand keeps a batch a few seconds of work.
const maxBatchFiles = 2000

// batches splits entries into sends bounded like a prefetch's, for an
// invalidation that arrives all at once (a `git checkout` is thousands of
// files). An entry with no Size is still bounded by maxBatchFiles.
func batches(entries []Entry) [][]Entry {
	var (
		out   [][]Entry
		batch []Entry
		size  int64
	)
	for _, e := range entries {
		if len(batch) > 0 && (size+e.Size > batchBytes || len(batch) >= maxBatchFiles) {
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
