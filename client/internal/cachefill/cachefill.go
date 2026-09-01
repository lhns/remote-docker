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
	"sync"
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

// SmallFile is the size below which a file is sent as the walk finds it,
// without waiting for the scan to finish.
//
// The reason this exists: sorting the whole tree before sending anything means
// the first byte waits for a stat of every file in the project, and on a large
// one that scan can take longer than the upload it was meant to optimise. A
// source tree is overwhelmingly files under this size, and those are the ones
// worth caching first anyway, so sending them immediately gets both -- an
// upload that starts at once AND the cheapest files first.
//
// 64 KiB: comfortably above a source file, well below anything whose transfer
// time is worth reordering for.
const SmallFile = 64 << 10

// Walk finds what to cache and hands each entry over as it is found.
//
// Streaming rather than returning a list, so a caller can start sending before
// the scan is done. Small files arrive in walk order and are worth sending
// straight away; everything else the caller should hold back and sort, which is
// what Plan does for callers that would rather have the whole answer.
//
// The budget is deliberately NOT applied here. A walk that stopped at the
// ceiling would let the DIRECTORY ORDER decide what a share caches: the first
// files it happened to reach, however large, and never the small ones behind
// them. The Selector holds the budget, because choosing needs to see the
// candidates.
//
// Errors are skipped rather than returned. A directory that cannot be read is a
// part of the tree that stays live, which is the same outcome as a budget that
// ran out, and refusing the whole share over one unreadable path would be worse
// than the thing it protects against.
func Walk(root string, excludes []string, yield func(Entry)) Stats {
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

// Sample is how many files are seen before the first batch is chosen.
//
// The number is not important and any low hundreds would do. What it is for is
// avoiding a first batch picked from whatever handful the walk reached first,
// which on an unlucky directory order would be the largest files in the tree.
// The scan reaches this in milliseconds, so it costs nothing to wait for.
const Sample = 100

// Selector holds the candidates a fill has found and hands out the smallest.
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
type Selector struct {
	budget Budget

	// entries is a max-heap by size: the largest is what eviction needs, and
	// the smallest is what draining needs, so the heap serves the operation
	// that happens per file and draining sorts, which happens per batch.
	entries []Entry
	bytes   int64

	// sentBytes and sentFiles are what has already gone, and they count
	// against the budget: the ceiling is on what a share caches in total, not
	// on what is waiting to be sent.
	sentBytes int64
	sentFiles int
}

// NewSelector returns one bounded by the budget.
func NewSelector(b Budget) *Selector { return &Selector{budget: b} }

// Add offers a file, and reports whether the buffer kept it.
//
// A file larger than everything held, with no room left, is refused rather than
// evicting something smaller -- which is the same decision as the eviction
// below, made without the pointless work of inserting it first.
func (s *Selector) Add(e Entry) bool {
	if s.wouldExceed(e) && (len(s.entries) == 0 || e.Size >= s.largest()) {
		return false
	}

	s.push(e)
	for s.over() && len(s.entries) > 0 {
		s.pop()
	}
	return true
}

// wouldExceed reports whether taking e as well would pass either bound, and
// over reports whether what is held already has.
//
// Two questions, deliberately not one function: asking "would one more fit"
// while evicting counts a file that is not there, and the first version did
// exactly that -- it evicted the only entry it held, every time.
func (s *Selector) wouldExceed(e Entry) bool {
	return s.sentFiles+len(s.entries)+1 > s.budget.files() ||
		s.sentBytes+s.bytes+e.Size > s.budget.bytes()
}

func (s *Selector) over() bool {
	return s.sentFiles+len(s.entries) > s.budget.files() ||
		s.sentBytes+s.bytes > s.budget.bytes()
}

// TakeSmallest removes and returns the cheapest files, up to a batch.
//
// Sorting here rather than keeping the buffer sorted: this happens once per
// batch, where the heap operations happen once per file.
func (s *Selector) TakeSmallest(maxBytes int64, maxFiles int) []Entry {
	if len(s.entries) == 0 {
		return nil
	}
	sort.SliceStable(s.entries, func(i, j int) bool { return s.entries[i].Size < s.entries[j].Size })

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
	s.heapify()
	return took
}

// Len is how many candidates are waiting.
func (s *Selector) Len() int { return len(s.entries) }

// Sent is what has been handed out, which is what the share has cached.
func (s *Selector) Sent() (files int, bytes int64) { return s.sentFiles, s.sentBytes }

// The heap, kept small and explicit rather than through container/heap: the
// interface's five methods on a named slice type are more code than this, for
// one use.
func (s *Selector) largest() int64 { return s.entries[0].Size }

func (s *Selector) push(e Entry) {
	s.entries = append(s.entries, e)
	s.bytes += e.Size
	for i := len(s.entries) - 1; i > 0; {
		parent := (i - 1) / 2
		if s.entries[parent].Size >= s.entries[i].Size {
			break
		}
		s.entries[parent], s.entries[i] = s.entries[i], s.entries[parent]
		i = parent
	}
}

func (s *Selector) pop() {
	last := len(s.entries) - 1
	s.bytes -= s.entries[0].Size
	s.entries[0] = s.entries[last]
	s.entries = s.entries[:last]
	s.sift(0)
}

func (s *Selector) heapify() {
	for i := len(s.entries)/2 - 1; i >= 0; i-- {
		s.sift(i)
	}
}

func (s *Selector) sift(i int) {
	for {
		largest, l, r := i, 2*i+1, 2*i+2
		if l < len(s.entries) && s.entries[l].Size > s.entries[largest].Size {
			largest = l
		}
		if r < len(s.entries) && s.entries[r].Size > s.entries[largest].Size {
			largest = r
		}
		if largest == i {
			return
		}
		s.entries[i], s.entries[largest] = s.entries[largest], s.entries[i]
		i = largest
	}
}

// MaxBatchFiles bounds a batch by count as well as by bytes.
//
// A batch of a hundred thousand empty files is small in bytes and slow in every
// other way: it is built in memory, framed, extracted entry by entry, and its
// failure costs all of it. Two thousand keeps a batch a few seconds of work.
const MaxBatchFiles = 2000

// Stream scans a tree and sends it at the same time, cheapest first.
//
// The scan feeds a Selector; the send loop drains it. They run together after
// the first Sample files, so the scan keeps improving the candidates while the
// upload works through them.
//
// send is called with batches in ascending size and must not be called
// concurrently -- it is not. It returning an error stops everything: a fill
// that cannot reach the workspace has nothing useful left to do, and the share
// is served from the lower meanwhile.
//
// It always terminates, including for a tree smaller than Sample, one that is
// empty, and one that cannot be read at all: the scan ending wakes the drain
// whether or not the sample was ever reached.
func Stream(root string, excludes []string, budget Budget, send func([]Entry) error) (Stats, error) {
	var (
		mu       sync.Mutex
		selector = NewSelector(budget)
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
		found := Walk(root, excludes, func(e Entry) {
			mu.Lock()
			selector.Add(e)
			mu.Unlock()

			// Nothing goes until the buffer has seen enough of the tree to be
			// choosing rather than guessing.
			if seen++; seen >= Sample {
				wake()
			}
		})

		mu.Lock()
		stats, scanning = found, false
		mu.Unlock()
		// Unconditional, and this is what makes a small tree work: one with
		// fewer than Sample files never woke the drain while scanning, so the
		// end of the scan is its first and only wake-up.
		wake()
	}()

	for range ready {
		for {
			mu.Lock()
			batch := selector.TakeSmallest(DefaultBatchBytes, MaxBatchFiles)
			done := !scanning && selector.Len() == 0
			result := stats
			mu.Unlock()

			if len(batch) > 0 {
				if err := send(batch); err != nil {
					return withSent(result, selector), err
				}
				continue
			}
			if done {
				return withSent(result, selector), nil
			}
			break
		}
	}
	return withSent(stats, selector), nil
}

// withSent fills in what was actually cached, which only the Selector knows:
// the walk counts what the tree HOLDS, and the budget decides how much of that
// is sent.
func withSent(stats Stats, s *Selector) Stats {
	stats.Files, stats.Bytes = s.Sent()
	return stats
}
