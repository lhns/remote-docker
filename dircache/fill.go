package dircache

// What goes into a cache, and in what order (ADR 0044).
//
// The cache is allowed to be incomplete at every moment: a file it does not
// hold is served from the authoritative tree underneath, correctly, just
// slowly. That single property turns every hard question into a cheap one -- a
// budget that ran out, a walk still running, a file skipped for being excluded,
// are all the same state as a file the fill has not reached yet.
//
// So nothing here can fail in a way that makes a share wrong. It can only make
// one slower.

import (
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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

// walk finds what to cache and hands each entry over as it is found.
//
// Streaming rather than returning a list, so a caller can start sending before
// the scan is done. Ordering what it yields is the selector's job.
//
// The budget is deliberately NOT applied here. A walk that stopped at the
// ceiling would let the DIRECTORY ORDER decide what a share caches: the first
// files it happened to reach, however large, and never the small ones behind
// them. The selector holds the budget, because choosing needs to see the
// candidates.
//
// Errors are skipped rather than returned. A directory that cannot be read is a
// part of the tree that stays live, which is the same outcome as a budget that
// ran out, and refusing the whole share over one unreadable path would be worse
// than the thing it protects against.
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
		// carry to another machine -- sockets, devices, pipes -- is not cached
		// at all. The live export answers for those.
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

// sample is how many files are seen before the first batch is chosen.
//
// The number is not important and any low hundreds would do. What it is for is
// avoiding a first batch picked from whatever handful the walk reached first,
// which on an unlucky directory order would be the largest files in the tree.
// The scan reaches this in milliseconds, so it costs nothing to wait for.
const sample = 100

// selector holds the candidates a fill has found and hands out the smallest.
//
// This is what lets the scan and the upload run at once while still sending the
// cheapest files first. A sorted list of the whole tree would be exact and
// would have to wait for the scan; this keeps the best candidates seen SO FAR,
// bounded by the same budget the fill has, and evicts the largest to make room
// for smaller ones as the scan goes on.
//
// So a file uploaded early may be larger than one found later. That is the
// price of not waiting, and it is small: the buffer holds everything that fits
// in the budget, so eviction only ever discards files that were not going to be
// sent anyway.
//
// Bounded by BOTH bytes and count. Bytes alone would let a tree of tiny files
// put twenty million entries in memory before the byte ceiling was anywhere
// near.
type selector struct {
	budget Budget

	// entries is kept sorted by size, ascending: draining takes a prefix and
	// eviction drops the tail, which is both operations in one invariant.
	entries []Entry
	bytes   int64

	// sentBytes and sentFiles are what has already gone, and they count
	// against the budget: the ceiling is on what a share caches in total, not
	// on what is waiting to be sent.
	sentBytes int64
	sentFiles int
}

// newSelector returns one bounded by the budget.
func newSelector(b Budget) *selector { return &selector{budget: b} }

// add offers a file, and reports whether the buffer kept it.
//
// A file larger than everything held, with no room left, is refused rather than
// evicting something smaller -- which is the same decision as the eviction
// below, made without the pointless work of inserting it first.
func (s *selector) add(e Entry) bool {
	if s.wouldExceed(e) && (len(s.entries) == 0 || e.Size >= s.largest()) {
		return false
	}

	at := sort.Search(len(s.entries), func(i int) bool { return s.entries[i].Size > e.Size })
	s.entries = append(s.entries, Entry{})
	copy(s.entries[at+1:], s.entries[at:])
	s.entries[at] = e
	s.bytes += e.Size

	for s.over() && len(s.entries) > 0 {
		last := len(s.entries) - 1
		s.bytes -= s.entries[last].Size
		s.entries = s.entries[:last]
	}
	return true
}

// wouldExceed reports whether taking e as well would pass either bound, and
// over reports whether what is held already has.
//
// Two questions, deliberately not one function: while evicting, "would one more
// fit" counts a file that is not there, so a buffer holding a single entry
// evicts it every time.
func (s *selector) wouldExceed(e Entry) bool {
	return s.sentFiles+len(s.entries)+1 > s.budget.files() ||
		s.sentBytes+s.bytes+e.Size > s.budget.bytes()
}

func (s *selector) over() bool {
	return s.sentFiles+len(s.entries) > s.budget.files() ||
		s.sentBytes+s.bytes > s.budget.bytes()
}

// takeSmallest removes and returns the cheapest files, up to a batch.
func (s *selector) takeSmallest(maxBytes int64, maxFiles int) []Entry {
	if len(s.entries) == 0 {
		return nil
	}

	var (
		took []Entry
		size int64
	)
	for _, e := range s.entries {
		if len(took) > 0 && (size+e.Size > maxBytes || len(took) >= maxFiles) {
			break
		}
		took = append(took, e)
		size += e.Size
	}

	s.entries = append([]Entry(nil), s.entries[len(took):]...)
	s.bytes -= size
	s.sentBytes += size
	s.sentFiles += len(took)
	return took
}

// waiting is how many candidates are waiting.
func (s *selector) waiting() int { return len(s.entries) }

// sent is what has been handed out, which is what the share has cached.
func (s *selector) totals() (files int, bytes int64) { return s.sentFiles, s.sentBytes }

// largest is the biggest file held, which is the tail of a sorted buffer.
func (s *selector) largest() int64 { return s.entries[len(s.entries)-1].Size }

// maxBatchFiles bounds a batch by count as well as by bytes.
//
// A batch of a hundred thousand empty files is small in bytes and slow in every
// other way: it is built in memory, framed, extracted entry by entry, and its
// failure costs all of it. Two thousand keeps a batch a few seconds of work.
const maxBatchFiles = 2000

// stream scans a tree and sends it at the same time, cheapest first.
//
// The scan feeds a selector; the send loop drains it. They run together after
// the first sample files, so the scan keeps improving the candidates while the
// upload works through them.
//
// send is called with batches in ascending size and must not be called
// concurrently -- it is not. It returning an error stops everything: a fill
// that cannot reach the workspace has nothing useful left to do, and the share
// is served from the lower meanwhile.
//
// It always terminates, including for a tree smaller than sample, one that is
// empty, and one that cannot be read at all: the scan ending wakes the drain
// whether or not the sample was ever reached.
func stream(root string, excludes []string, budget Budget, send func([]Entry) error) (Stats, error) {
	var (
		mu       sync.Mutex
		sel      = newSelector(budget)
		scanning = true
		stats    Stats
		ready    = make(chan struct{}, 1)
	)

	wake := func() {
		select {
		case ready <- struct{}{}:
		default:
		}
	}

	go func() {
		seen := 0
		found := walk(root, excludes, func(e Entry) {
			mu.Lock()
			sel.add(e)
			mu.Unlock()

			// Nothing goes until the buffer has seen enough of the tree to be
			// choosing rather than guessing.
			if seen++; seen >= sample {
				wake()
			}
		})

		mu.Lock()
		stats, scanning = found, false
		mu.Unlock()
		// Unconditional, and this is what makes a small tree work: one with
		// fewer than sample files never woke the drain while scanning, so the
		// end of the scan is its first and only wake-up.
		wake()
	}()

	for range ready {
		for {
			mu.Lock()
			batch := sel.takeSmallest(batchBytes, maxBatchFiles)
			done := !scanning && sel.waiting() == 0
			result := stats
			mu.Unlock()

			if len(batch) > 0 {
				if err := send(batch); err != nil {
					return withSent(result, sel), err
				}
				continue
			}
			if done {
				return withSent(result, sel), nil
			}
			break
		}
	}
	return withSent(stats, sel), nil
}

// withSent fills in what was actually cached, which only the selector knows:
// the walk counts what the tree HOLDS, and the budget decides how much of that
// is sent.
func withSent(stats Stats, s *selector) Stats {
	stats.Files, stats.Bytes = s.totals()
	return stats
}

// batches splits entries into sends bounded the same way a fill's are.
//
// For the paths an invalidation carries, which arrive all at once rather than
// through the selector: a `git checkout` across a branch, or a build that
// rewrote a generated directory, is thousands of files in one event. sent as
// one batch it is one tar held whole in memory, framed as one payload, and
// lost entirely if anything about it fails.
//
// An entry with no Size counts as nothing against the byte budget, so a caller
// that has not stat'ed its paths is still bounded by maxBatchFiles.
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
