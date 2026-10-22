package dircache

// What write-back remembers about a share between rounds (ADR 0044): when it
// may try again after a failure, and which paths it has given up on until they
// change. All of it is read and written under shares.mu.

import (
	"slices"
	"sort"
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

	// Why is the error, as a person reads it.
	Why string

	// Link and Clash say what kind of path it is, for status to group them: a
	// link this account may not create, and a name that differs only in case
	// from one already here.
	Link  bool
	Clash bool
}

// skip is a path given up on and the change that was refused, so a later
// change to the same path is tried again.
type skip struct {
	change Change
	Skipped
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
	skipped   map[string]skip

	// said is the conflict last reported for each path, so a file kept as a
	// conflict is said once and not every round. Forgotten when the path lands.
	said map[string]string

	// tried counts the rounds a path was asked for and did not land.
	tried map[string]int
}

// same reports whether a change is the one that was refused.
func (s skip) same(c Change) bool {
	return s.change.Size == c.Size && s.change.ModTime == c.ModTime &&
		s.change.Deleted == c.Deleted && s.change.Link == c.Link
}

// wbReady reports whether a share may be tried at now.
func (f *shares) wbReady(share string, now time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.state[share]
	return ok && !now.Before(s.wb.next)
}

// wbFail records a round that could not finish. It reports whether the share
// has just stalled, which is when it is said once, and the time of the next
// try. A round that made progress starts the backoff over.
func (f *shares) wbFail(share string, now time.Time, reason, first string, pending int, progress bool) (newly bool, next time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.state[share]
	if !ok {
		return false, time.Time{}
	}
	w := &s.wb
	if progress {
		w.failures = 0
	}
	delay := writeBackBackoffMin
	for i := 0; i < w.failures && delay < writeBackBackoffMax; i++ {
		delay *= 2
	}
	delay = min(delay, writeBackBackoffMax)
	w.failures++
	w.next = now.Add(delay)
	newly = !w.stalled
	if newly {
		w.since = now
	}
	w.stalled, w.reason, w.firstPath, w.pending = true, reason, first, pending
	return newly, w.next
}

// wbOK records a round that finished, and reports when it ended a stall.
func (f *shares) wbOK(share string) (recovered bool, since time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.state[share]
	if !ok {
		return false, time.Time{}
	}
	w := &s.wb
	recovered, since = w.stalled, w.since
	w.failures, w.next, w.stalled, w.reason, w.firstPath, w.pending = 0, time.Time{}, false, "", "", 0
	return recovered, since
}

// wbSkip gives up on a path until its change differs, and reports whether it is
// new, which is when it is said once.
func (f *shares) wbSkip(share string, ch Change, sk Skipped) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.state[share]
	if !ok {
		return false
	}
	if s.wb.skipped == nil {
		s.wb.skipped = map[string]skip{}
	}
	old, had := s.wb.skipped[ch.Path]
	sk.Path = ch.Path
	s.wb.skipped[ch.Path] = skip{change: ch, Skipped: sk}
	return !had || !old.same(ch)
}

// wbConflictNew reports whether a conflict is new, and remembers it.
func (f *shares) wbConflictNew(share, path, why string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.state[share]
	if !ok {
		return false
	}
	if s.wb.said == nil {
		s.wb.said = map[string]string{}
	}
	if s.wb.said[path] == why {
		return false
	}
	s.wb.said[path] = why
	return true
}

// wbForgetSaid forgets the conflicts of paths no longer pending, so one that
// comes back after it was settled is said again.
func (f *shares) wbForgetSaid(share string, changes []Change) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.state[share]
	if !ok || len(s.wb.said) == 0 {
		return
	}
	pending := make(map[string]bool, len(changes))
	for _, ch := range changes {
		pending[ch.Path] = true
	}
	for p := range s.wb.said {
		if !pending[p] {
			delete(s.wb.said, p)
		}
	}
}

// wbRanked orders paths by how often they were asked for and did not land,
// fewest first and otherwise as given. A path no longer pending is forgotten.
func (f *shares) wbRanked(share string, paths []string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.state[share]
	if !ok || len(s.wb.tried) == 0 {
		return paths
	}
	pending := make(map[string]bool, len(paths))
	for _, p := range paths {
		pending[p] = true
	}
	for p := range s.wb.tried {
		if !pending[p] {
			delete(s.wb.tried, p)
		}
	}
	out := slices.Clone(paths)
	slices.SortStableFunc(out, func(a, b string) int { return s.wb.tried[a] - s.wb.tried[b] })
	return out
}

// wbTry records a pass: a path asked for that landed forgets its count, one that
// did not adds to it.
func (f *shares) wbTry(share string, asked, landed map[string]bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.state[share]
	if !ok {
		return
	}
	if s.wb.tried == nil {
		s.wb.tried = map[string]int{}
	}
	for p := range asked {
		if landed[p] {
			delete(s.wb.tried, p)
		} else {
			s.wb.tried[p]++
		}
	}
}

// wbUnskip forgets paths that have now landed: a path given up on and then
// changed is tried again, and once it goes through it is no longer given up on.
func (f *shares) wbUnskip(share string, paths ...[]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.state[share]
	if !ok {
		return
	}
	for _, list := range paths {
		for _, p := range list {
			delete(s.wb.skipped, p)
			delete(s.wb.said, p)
		}
	}
}

// unskipped removes from changes what was given up on and has not changed
// since, and forgets a skipped path that is no longer reported at all.
func (f *shares) unskipped(share string, changes []Change) []Change {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.state[share]
	if !ok || len(s.wb.skipped) == 0 {
		return changes
	}
	present := make(map[string]bool, len(changes))
	out := make([]Change, 0, len(changes))
	for _, ch := range changes {
		present[ch.Path] = true
		if sk, skipped := s.wb.skipped[ch.Path]; skipped && sk.same(ch) {
			continue
		}
		out = append(out, ch)
	}
	for p := range s.wb.skipped {
		if !present[p] {
			delete(s.wb.skipped, p)
		}
	}
	return out
}

// wbReport is a share's write-back as status shows it.
func (s *shareState) wbReport() WriteBackReport {
	r := WriteBackReport{
		Stalled: s.wb.stalled, Since: s.wb.since, Next: s.wb.next,
		Reason: s.wb.reason, FirstPath: s.wb.firstPath, Pending: s.wb.pending,
	}
	for _, sk := range s.wb.skipped {
		r.Skipped = append(r.Skipped, sk.Skipped)
	}
	sort.Slice(r.Skipped, func(i, j int) bool { return r.Skipped[i].Path < r.Skipped[j].Path })
	return r
}
