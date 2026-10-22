package dircache

// What write-back remembers about a share between rounds (ADR 0044): when it
// may try again after a failure, and which paths it has given up on until they
// change.

import (
	"maps"
	"slices"
	"sort"
	"sync"
	"time"
)

const (
	// writeBackBackoffMin and writeBackBackoffMax bound the wait after a round
	// that failed. The first retry is soon, since a disk that was full a moment
	// ago may not be; a share that stays broken is asked about once a minute,
	// which costs nothing and recovers within one.
	writeBackBackoffMin = 5 * time.Second
	writeBackBackoffMax = time.Minute
)

// WriteBackReport is what status says about a share's write-back.
type WriteBackReport struct {
	// Stalled says the last round could not finish and another is waiting for
	// Next. Since is when it first failed, Reason why, FirstPath the first
	// file that did not land (empty when no file was the cause) and Pending
	// how many changes were left, or -1 when that is not known.
	Stalled   bool
	Since     time.Time
	Next      time.Time
	Reason    string
	FirstPath string
	Pending   int

	// Skipped are the paths given up on until they change.
	Skipped []Skipped
}

// Skipped is one path write-back will not retry.
type Skipped struct {
	Path string

	// Why is the error, as a person reads it, and Kind what status groups it
	// under, since a person needs to know what to do about each.
	Why  string
	Kind SkipKind
}

// SkipKind is why write-back gave up on a path.
type SkipKind int

const (
	// SkipName is a path this machine cannot hold, or one it could not read.
	SkipName SkipKind = iota

	// SkipLink is a link this account may not create here.
	SkipLink

	// SkipClash is a name that differs only in case from one already here.
	SkipClash

	// SkipMarker is a name the union reserves for its own bookkeeping.
	SkipMarker
)

// skip is a path given up on and the change that was refused, so a later
// change to the same path is tried again.
type skip struct {
	change Change
	Skipped
}

// writeBacks is write-back's memory of every share, under a lock of its own so
// a round's bookkeeping never waits on the fill.
//
// A share has an entry exactly while it is attached (shares.set, shares.forget)
// and every method is a no-op without one, so a round that ends after the share
// was forgotten leaves nothing for the next attach to inherit.
//
// Lock order: shares.mu may be held while taking writeBacks.mu, never the reverse.
type writeBacks struct {
	mu    sync.Mutex
	state map[string]*wbState
}

// wbState is one share's write-back memory.
type wbState struct {
	failures  int
	next      time.Time
	stalled   bool
	since     time.Time
	reason    string
	firstPath string
	pending   int

	// Per path, each forgotten once the path lands or stops being pending
	// (landed, forget): the change given up on, the conflict last said (so a
	// conflict kept is said once and not every round), and how many passes
	// asked for it without it landing.
	skipped map[string]skip
	said    map[string]string
	tried   map[string]int
}

// same reports whether a change is the one that was refused.
func (s skip) same(c Change) bool {
	return s.change.Size == c.Size && s.change.ModTime == c.ModTime &&
		s.change.Deleted == c.Deleted && s.change.Link == c.Link
}

// backoff is the wait after failures in a row: min, doubling, at most max.
func backoff(minWait, maxWait time.Duration, failures int) time.Duration {
	d := minWait
	for i := 0; i < failures && d < maxWait; i++ {
		d *= 2
	}
	return min(d, maxWait)
}

// reset gives a share a fresh memory, when it is attached.
func (w *writeBacks) reset(share string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.state == nil {
		w.state = map[string]*wbState{}
	}
	w.state[share] = &wbState{}
}

// drop forgets a share, when the cache does.
func (w *writeBacks) drop(share string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.state, share)
}

// ready reports whether a share may be tried at now.
func (w *writeBacks) ready(share string, now time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	s, ok := w.state[share]
	return ok && !now.Before(s.next)
}

// fail records a round that could not finish. It reports whether the share
// has just stalled, which is when it is said once, and the time of the next
// try. A round that made progress starts the backoff over.
func (w *writeBacks) fail(share string, now time.Time, reason, first string, pending int, progress bool) (newly bool, next time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	s, ok := w.state[share]
	if !ok {
		return false, time.Time{}
	}
	if progress {
		s.failures = 0
	}
	s.next = now.Add(backoff(writeBackBackoffMin, writeBackBackoffMax, s.failures))
	s.failures++
	newly = !s.stalled
	if newly {
		s.since = now
	}
	s.stalled, s.reason, s.firstPath, s.pending = true, reason, first, pending
	return newly, s.next
}

// ok records a round that finished, and reports when it ended a stall.
func (w *writeBacks) ok(share string) (recovered bool, since time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	s, ok := w.state[share]
	if !ok {
		return false, time.Time{}
	}
	recovered, since = s.stalled, s.since
	s.failures, s.next, s.stalled, s.reason, s.firstPath, s.pending = 0, time.Time{}, false, "", "", 0
	return recovered, since
}

// skip gives up on a path until its change differs, and reports whether it is
// new, which is when it is said once.
func (w *writeBacks) skip(share string, ch Change, sk Skipped) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	s, ok := w.state[share]
	if !ok {
		return false
	}
	if s.skipped == nil {
		s.skipped = map[string]skip{}
	}
	old, had := s.skipped[ch.Path]
	sk.Path = ch.Path
	s.skipped[ch.Path] = skip{change: ch, Skipped: sk}
	return !had || !old.same(ch)
}

// conflictNew reports whether a conflict is new, and remembers it.
func (w *writeBacks) conflictNew(share, path, why string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	s, ok := w.state[share]
	if !ok {
		return false
	}
	if s.said == nil {
		s.said = map[string]string{}
	}
	if s.said[path] == why {
		return false
	}
	s.said[path] = why
	return true
}

// ranked orders paths by how often they were asked for and did not land,
// fewest first and otherwise as given.
func (w *writeBacks) ranked(share string, paths []string) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	s, ok := w.state[share]
	if !ok || len(s.tried) == 0 {
		return paths
	}
	out := slices.Clone(paths)
	slices.SortStableFunc(out, func(a, b string) int { return s.tried[a] - s.tried[b] })
	return out
}

// try records a pass: each path asked for that did not land adds to its count.
func (w *writeBacks) try(share string, asked, landed map[string]bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	s, ok := w.state[share]
	if !ok {
		return
	}
	if s.tried == nil {
		s.tried = map[string]int{}
	}
	for p := range asked {
		if !landed[p] {
			s.tried[p]++
		}
	}
}

// landed forgets everything remembered about paths that have now landed: a
// path given up on and then changed is tried again, and once it goes through it
// is no longer given up on.
func (w *writeBacks) landed(share string, paths ...[]string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	s, ok := w.state[share]
	if !ok {
		return
	}
	for _, list := range paths {
		for _, p := range list {
			delete(s.skipped, p)
			delete(s.said, p)
			delete(s.tried, p)
		}
	}
}

// unskipped removes from changes what was given up on and has not changed
// since.
func (w *writeBacks) unskipped(share string, changes []Change) []Change {
	w.mu.Lock()
	defer w.mu.Unlock()
	s, ok := w.state[share]
	if !ok || len(s.skipped) == 0 {
		return changes
	}
	out := make([]Change, 0, len(changes))
	for _, ch := range changes {
		if sk, skipped := s.skipped[ch.Path]; skipped && sk.same(ch) {
			continue
		}
		out = append(out, ch)
	}
	return out
}

// forget forgets what write-back remembers about every path no longer pending:
// the conflict said, the failed tries, the change given up on. A path that
// comes back after it was settled is then said, tried and judged afresh.
func (w *writeBacks) forget(share string, pending ...[]Change) {
	w.mu.Lock()
	defer w.mu.Unlock()
	s, ok := w.state[share]
	if !ok {
		return
	}
	keep := map[string]bool{}
	for _, list := range pending {
		for _, ch := range list {
			keep[ch.Path] = true
		}
	}
	maps.DeleteFunc(s.said, func(p, _ string) bool { return !keep[p] })
	maps.DeleteFunc(s.tried, func(p string, _ int) bool { return !keep[p] })
	maps.DeleteFunc(s.skipped, func(p string, _ skip) bool { return !keep[p] })
}

// report is a share's write-back as status shows it.
func (w *writeBacks) report(share string) WriteBackReport {
	w.mu.Lock()
	defer w.mu.Unlock()
	s, ok := w.state[share]
	if !ok {
		return WriteBackReport{}
	}
	r := WriteBackReport{
		Stalled: s.stalled, Since: s.since, Next: s.next,
		Reason: s.reason, FirstPath: s.firstPath, Pending: s.pending,
	}
	for _, sk := range s.skipped {
		r.Skipped = append(r.Skipped, sk.Skipped)
	}
	sort.Slice(r.Skipped, func(i, j int) bool { return r.Skipped[i].Path < r.Skipped[j].Path })
	return r
}
