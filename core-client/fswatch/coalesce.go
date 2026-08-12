package fswatch

import (
	"path"
	"sort"
	"time"

	"github.com/lhns/remote-docker/core/workspace"
)

// Defaults for coalescing. A single editor save produces three to five raw
// inotify events for one file, and a build produces thousands across a tree;
// the agent pays a syscall for each one it is told about, so collapsing them
// here is the difference between a notification channel and a denial of
// service aimed at the workspace.
const (
	// DefaultDebounce is how long a path must be quiet before it is reported.
	DefaultDebounce = 40 * time.Millisecond

	// DefaultMaxDelay caps how long a continuously-touched path can be held
	// back. Without it, a file appended to in a loop (a log, a compiler
	// writing output) would never be reported at all, because it is
	// never quiet for a whole debounce interval.
	DefaultMaxDelay = 250 * time.Millisecond

	// DefaultPendingCap bounds the coalescer's own memory. Reaching it is an
	// overflow like any other and produces a notice; it is not a silent drop.
	DefaultPendingCap = 8192
)

type pendingKey struct {
	export string
	path   string
}

type pendingEvent struct {
	op  workspace.FSOp
	dir bool

	// seq is the arrival order of the FIRST event for this path, so a flush
	// preserves the only ordering that matters: a directory's creation always
	// precedes its children's, because it necessarily arrived first.
	seq uint64

	first time.Time
	last  time.Time
}

// lost accumulates what could not be kept, per export, so one notice can stand
// for many dropped events.
type lost struct {
	count int
	dir   string // deepest directory covering everything lost
}

// coalescer merges filesystem events per path and decides when each is due.
//
// It owns no goroutine, no timer and no clock: every method takes the current
// time from the caller. That makes the whole of the interesting behaviour --
// merge rules, due-ness, overflow and ordering testable by calling methods
// with made-up timestamps, on a development machine with no daemon and no
// kernel to observe.
type coalescer struct {
	debounce time.Duration
	maxDelay time.Duration
	cap      int

	pending map[pendingKey]*pendingEvent
	lost    map[string]*lost
	seq     uint64
}

func newCoalescer(debounce, maxDelay time.Duration, capacity int) *coalescer {
	if debounce <= 0 {
		debounce = DefaultDebounce
	}
	if maxDelay <= 0 {
		maxDelay = DefaultMaxDelay
	}
	if capacity <= 0 {
		capacity = DefaultPendingCap
	}
	return &coalescer{
		debounce: debounce,
		maxDelay: maxDelay,
		cap:      capacity,
		pending:  make(map[pendingKey]*pendingEvent),
		lost:     make(map[string]*lost),
	}
}

// add merges one observed event into the pending set.
func (c *coalescer) add(now time.Time, e workspace.FSEvent) {
	key := pendingKey{e.Export, e.Path}
	if p, ok := c.pending[key]; ok {
		p.op = mergeOps(p.op, e.Op)
		p.dir = p.dir || e.Dir
		p.last = now
		return
	}

	if len(c.pending) >= c.cap {
		c.drop(e.Export, e.Path)
		return
	}

	c.seq++
	c.pending[key] = &pendingEvent{op: e.Op, dir: e.Dir, seq: c.seq, first: now, last: now}
}

// mergeOps combines an accumulated operation set with a newly observed one.
//
// Removal and rename are exclusive with creation and writing rather than
// additive, and which wins is decided by arrival order, because both orders
// occur and mean different things. Create-then-remove within one window is a
// temporary file, and the net truth is that it is gone. Remove-then-create is
// the atomic-save every editor performs, and the net truth is that it is
// there. Merging them by OR would report both and the agent would have to
// guess.
func mergeOps(have, add workspace.FSOp) workspace.FSOp {
	const gone = workspace.OpRemove | workspace.OpRename
	const present = workspace.OpCreate | workspace.OpWrite

	switch {
	case add&gone != 0:
		return (have &^ present) | add
	case add&present != 0:
		return (have &^ gone) | add
	default:
		return have | add
	}
}

// drop records an event that could not be kept. Individual paths are not
// remembered (that is the whole point of being over capacity) only how
// many were lost and the deepest directory covering all of them.
func (c *coalescer) drop(export, p string) {
	l, ok := c.lost[export]
	if !ok {
		l = &lost{dir: path.Dir(p)}
		c.lost[export] = l
	} else {
		l.dir = commonDir(l.dir, path.Dir(p))
	}
	l.count++
}

// overflow records a loss the caller detected elsewhere: a kernel queue
// overflow, a full outbound queue, a refused watch. Same accounting, so there
// is one degradation path rather than several that must agree.
func (c *coalescer) overflow(export, dir string, n int) {
	l, ok := c.lost[export]
	if !ok {
		c.lost[export] = &lost{count: n, dir: dir}
		return
	}
	l.dir = commonDir(l.dir, dir)
	l.count += n
}

// commonDir is the deepest directory containing both paths. Both are
// in-share paths: absolute, "/"-separated, "/" at the root.
func commonDir(a, b string) string {
	if a == b {
		return a
	}
	ap := splitSlash(a)
	bp := splitSlash(b)
	n := min(len(ap), len(bp))
	i := 0
	for i < n && ap[i] == bp[i] {
		i++
	}
	if i == 0 {
		return "/"
	}
	return "/" + joinSlash(ap[:i])
}

func splitSlash(p string) []string {
	var parts []string
	start := 0
	for i := 0; i <= len(p); i++ {
		if i == len(p) || p[i] == '/' {
			if i > start {
				parts = append(parts, p[start:i])
			}
			start = i + 1
		}
	}
	return parts
}

func joinSlash(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "/"
		}
		out += p
	}
	return out
}

// due reports whether a pending entry should be flushed at now: either it has
// been quiet for a debounce interval, or it has been held for the maximum
// delay and must be reported regardless.
func (c *coalescer) due(now time.Time, p *pendingEvent) bool {
	return now.Sub(p.last) >= c.debounce || now.Sub(p.first) >= c.maxDelay
}

// nextDue is when the earliest pending entry becomes due, for arming a timer.
// The second result is false when nothing is pending.
func (c *coalescer) nextDue(now time.Time) (time.Time, bool) {
	var earliest time.Time
	for _, p := range c.pending {
		at := p.last.Add(c.debounce)
		if byDelay := p.first.Add(c.maxDelay); byDelay.Before(at) {
			at = byDelay
		}
		if at.Before(now) {
			at = now
		}
		if earliest.IsZero() || at.Before(earliest) {
			earliest = at
		}
	}
	return earliest, !earliest.IsZero()
}

// flush returns everything due at now, in arrival order, plus a notice for any
// export that lost events since the last flush.
//
// Notices are emitted alongside the events rather than instead of them: what
// survived is still worth replaying, and the notice says the picture is
// incomplete.
func (c *coalescer) flush(now time.Time) ([]workspace.FSEvent, []workspace.FSNotice) {
	type ready struct {
		key pendingKey
		ev  *pendingEvent
	}
	var out []ready
	for key, p := range c.pending {
		if c.due(now, p) {
			out = append(out, ready{key, p})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ev.seq < out[j].ev.seq })

	events := make([]workspace.FSEvent, 0, len(out))
	for _, r := range out {
		delete(c.pending, r.key)
		events = append(events, workspace.FSEvent{
			Export: r.key.export,
			Path:   r.key.path,
			Op:     r.ev.op,
			Dir:    r.ev.dir,
		})
	}

	var notices []workspace.FSNotice
	if len(c.lost) > 0 {
		exports := make([]string, 0, len(c.lost))
		for export := range c.lost {
			exports = append(exports, export)
		}
		sort.Strings(exports)
		for _, export := range exports {
			l := c.lost[export]
			notices = append(notices, workspace.FSNotice{
				Export:  export,
				Path:    l.dir,
				Dropped: l.count,
				Reason:  "overflow",
			})
		}
		clear(c.lost)
	}

	return events, notices
}

// pendingCount is how many paths are held back, for Stats.
func (c *coalescer) pendingCount() int { return len(c.pending) }
