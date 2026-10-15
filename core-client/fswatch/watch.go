// Package fswatch watches the directories this client shares and reports what
// changed, so the agent can replay each change as a real syscall inside the
// workspace and the kernel there emits a genuine inotify event.
//
// The client is the right place to watch: the files are local, notification
// works natively on every platform we ship, and the workspace cannot observe
// them at all. NFS carries no change notification, which is the whole of
// ADR 0014. See ADR 0016 for what the agent then does.
package fswatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/lhns/remote-docker/core/logx"
	"github.com/lhns/remote-docker/core/notify"
	"github.com/lhns/remote-docker/core/workspace"
)

// Share is one export and the local directory behind it.
//
// Deliberately not *nfsserve.Share: the same decoupling as rewrite.Sharer, so
// this package does not import the NFS server in order to be tested.
type Share struct {
	ExportPath string
	LocalPath  string

	// Mode is how much of this share's changes are worth sending: partial
	// sends only what the agent can replay faithfully (writes and creations),
	// coarse also sends deletions and renames as a poke at the parent
	// directory (ADR 0016). A share is watched by its own mode, so one watcher
	// serves shares that disagree; off, or unset, leaves it unwatched.
	Mode workspace.Watch

	// File is the base name when this share exports a single file rather than
	// a directory (ADR 0039). The watch then goes on the CONTAINING directory,
	// because that is where the events for a file arrive -- an editor writing
	// through a temporary file replaces the inode, and a watch on the old one
	// sees nothing -- and everything but that name is dropped.
	File string
}

// Sink carries frames to the agent. Nil while no connection is established.
type Sink interface {
	Send(ctx context.Context, frame notify.Frame) error
}

// DefaultQueueLen bounds the frames waiting to be sent. A full queue is an
// overflow, not a stall: blocking back into the event drain would cause a
// kernel-level IN_Q_OVERFLOW, which is both worse and less informative than
// saying so ourselves.
const DefaultQueueLen = 256

// Options configures a Watcher. The zero value of every field is a working
// default.
type Options struct {
	Budget   int
	Exclude  []string
	Debounce time.Duration
	MaxDelay time.Duration
	QueueLen int
	Log      *slog.Logger

	// goos and newBackend are test seams: they let the Windows and macOS path
	// rules, and the whole of the watch bookkeeping, be exercised from a Linux
	// CI runner with no kernel watches involved.
	goos       string
	newBackend func() (backend, error)
}

// Stats is what the watcher is doing: everything it dropped appears here,
// because a cap nobody can see is a cap that lies. Nothing in the client reads
// it (`status` reports the dircache's own figures); it is how the tests see the
// budget, the exclude list and overflow reporting.
type Stats struct {
	Connected  bool
	Watched    int
	Budget     int
	Denied     int
	DeniedDirs []string
	Excluded   int
	Pending    int
	Sent       uint64
	Dropped    uint64
}

// Watcher watches every registered share and streams what changed.
//
// Its lifetime is the Session's, not a single connection's: watches are a
// local resource and re-walking a ten-thousand-directory tree on every idle
// reconnect would cost more than the connection it followed. The sink comes
// and goes with the connection instead.
type Watcher struct {
	opts Options
	be   backend

	// raw decouples draining the backend from processing. The drain goroutine
	// does nothing but move events, so a slow walk in the processor cannot
	// back up into the kernel's own queue.
	raw   chan fsnotify.Event
	syncC chan []Share

	// missed says events were lost before any share could be named: the
	// kernel's queue overflowed, or raw was full.
	missed chan struct{}

	mu           sync.Mutex
	sink         Sink
	observer     Observer
	stats        Stats
	disconnected map[string]int

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	closed sync.Once
}

// New starts a watcher. It watches nothing until Sync names shares, and what it
// does with each is that share's own Mode.
func New(opts Options) (*Watcher, error) {
	if opts.goos == "" {
		opts.goos = runtime.GOOS
	}
	if opts.Budget <= 0 {
		opts.Budget = DefaultBudget()
	}
	opts.Exclude = ExcludesOr(opts.Exclude)
	if opts.QueueLen <= 0 {
		opts.QueueLen = DefaultQueueLen
	}
	if opts.newBackend == nil {
		opts.newBackend = newFsnotifyBackend
	}

	ctx, cancel := context.WithCancel(context.Background())
	w := &Watcher{
		opts:         opts,
		raw:          make(chan fsnotify.Event, 4096),
		syncC:        make(chan []Share, 1),
		missed:       make(chan struct{}, 1),
		disconnected: make(map[string]int),
		ctx:          ctx,
		cancel:       cancel,
	}
	w.stats.Budget = opts.Budget

	be, err := opts.newBackend()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("fswatch: starting the file watcher: %w", err)
	}
	w.be = be

	out := make(chan notify.Frame, opts.QueueLen)
	w.wg.Go(w.drain)
	w.wg.Go(func() { w.process(out) })
	w.wg.Go(func() { w.send(out) })
	return w, nil
}

// Sync tells the watcher which shares exist now. Idempotent and diff-based, so
// calling it from both the registration path and a periodic reconcile costs
// nothing.
func (w *Watcher) Sync(shares []Share) {
	// Non-blocking with replacement: only the latest set matters, and a
	// caller reconciling on a ticker must never be held up by a walk.
	select {
	case <-w.syncC:
	default:
	}
	select {
	case w.syncC <- shares:
	default:
	}
}

// Observer is told about every change before the mode strips any: partial
// drops a deletion the Sink cannot replay, and a cache must have exactly that
// event, or a cached copy shadows a file that is gone (ADR 0044).
type Observer interface {
	Observe(event notify.Event)

	// Lost says events were missed, deletions possibly among them; a cache
	// answers with a reconcile.
	Lost(notice notify.Notice)
}

// SetObserver attaches one, replacing any before it. Nil detaches.
func (w *Watcher) SetObserver(o Observer) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.observer = o
}

// lost tells the observer that something was missed, if there is one.
func (w *Watcher) lost(notice notify.Notice) {
	if o := w.current(); o != nil {
		o.Lost(notice)
	}
}

// observe tells the observer, if there is one.
func (w *Watcher) observe(event notify.Event) {
	if o := w.current(); o != nil {
		o.Observe(event)
	}
}

func (w *Watcher) current() Observer {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.observer
}

// SetSink attaches a connection; nil detaches it without disturbing the
// watches. Anything dropped while there was none is reported first, so the
// agent knows its picture starts incomplete.
func (w *Watcher) SetSink(s Sink) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sink = s
	w.stats.Connected = s != nil
}

func (w *Watcher) Stats() Stats {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stats
}

func (w *Watcher) Close() error {
	w.closed.Do(func() {
		w.cancel()
		if w.be != nil {
			_ = w.be.Close()
		}
	})
	w.wg.Wait()
	return nil
}

// drain moves backend events into the internal queue and does nothing else.
func (w *Watcher) drain() {
	for {
		select {
		case <-w.ctx.Done():
			return
		case e, ok := <-w.be.Events():
			if !ok {
				return
			}
			select {
			case w.raw <- e:
			default:
				// The processor is behind. Recording this as an overflow is
				// the whole point: a receiver that silently believes it has
				// seen everything is the failure this package removes.
				w.countDropped(1)
				w.miss()
			}
		case err, ok := <-w.be.Errors():
			if !ok {
				return
			}
			w.log().Warn("file watcher", "err", err)
			if errors.Is(err, fsnotify.ErrEventOverflow) {
				w.miss()
			}
		}
	}
}

func (w *Watcher) miss() {
	select {
	case w.missed <- struct{}{}:
	default:
	}
}

// process owns the tree and the coalescer exclusively, so neither needs a lock.
func (w *Watcher) process(out chan<- notify.Frame) {
	defer close(out)

	t := newTree(w.opts.goos, w.be, w.opts.Budget, w.opts.Exclude, w.opts.Log)
	c := newCoalescer(w.opts.Debounce, w.opts.MaxDelay, 0)

	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	armed := false

	arm := func() {
		at, ok := c.nextDue(time.Now())
		if !ok {
			return
		}
		if !armed {
			timer.Reset(time.Until(at))
			armed = true
		}
	}

	for {
		select {
		case <-w.ctx.Done():
			return

		case shares := <-w.syncC:
			t.sync(shares)
			w.publishTree(t, c)

		case e := <-w.raw:
			w.handle(t, c, e)
			arm()

		case <-w.missed:
			// Which share lost what is unknown, so every one is told it lost
			// something somewhere, which is what a cache reconciles on.
			for _, r := range t.roots {
				c.overflow(r.export, "/", 0)
			}
			if !armed {
				timer.Reset(0)
				armed = true
			}

		case <-timer.C:
			armed = false
			events, notices := c.flush(time.Now())
			w.emit(out, events, notices)
			// Published here rather than on every event: the tree changes
			// whenever a directory is created or removed, and taking the
			// stats lock per event would put a mutex on the hot path to
			// keep a number nobody is reading at that instant.
			w.publishTree(t, c)
			arm()
		}
	}
}

// handle turns one backend event into pending work, and keeps the watched set
// in step with what just happened to the tree.
func (w *Watcher) handle(t *tree, c *coalescer, e fsnotify.Event) {
	roots := t.rootsFor(e.Name)
	if len(roots) == 0 {
		// An event for a path under no current share. Ordinary during a
		// resync, and nothing can be done with it.
		return
	}

	isDir := t.watching(e.Name)

	switch {
	case e.Has(fsnotify.Create):
		// A directory that appeared must be watched, and re-walked: anything
		// created inside it between the mkdir and our watch landing produced
		// no event and would otherwise be lost entirely.
		// Lstat, never Stat: inotify_add_watch on a symlink watches its
		// TARGET, which would let a watch escape the share that
		// osfs.WithBoundOS() exists to bound, and admits cycles.
		if info, err := os.Lstat(e.Name); err == nil && info.IsDir() {
			isDir = true
			if !t.isExcluded(filepath.Base(e.Name)) {
				t.addTree(roots[0].root, e.Name, func(p string, dir bool) {
					for _, m := range roots {
						if childRel, ok := relativeTo(w.opts.goos, m.root.parts, p); ok {
							c.add(time.Now(), notify.Event{
								Export: m.root.export, Path: childRel,
								Op: notify.OpCreate, Dir: dir,
							})
						}
					}
				})
			}
		}

	case e.Has(fsnotify.Rename):
		// Watches follow the inode, so events would keep arriving under the
		// old path. Drop the whole subtree; the matching create at the new
		// location re-adds it.
		if isDir {
			t.removeTree(e.Name)
		}

	case e.Has(fsnotify.Remove):
		// One IN_DELETE_SELF arrives per watched descendant, so a single
		// removal here keeps `rm -rf` linear rather than quadratic.
		if isDir {
			t.removeOne(e.Name)
		}
	}

	op := opFor(e)
	if op == 0 {
		return
	}

	// Every root at the path gets the event, each by its own mode (a directory
	// mounted several ways is several roots, ADR 0042).
	for _, m := range roots {
		w.deliver(c, m.root, m.rel, op, isDir)
	}
}

// deliver reports one event to one share.
func (w *Watcher) deliver(c *coalescer, root *shareRoot, rel string, op notify.Op, isDir bool) {
	// Delivered before the mode strips anything, and that is the difference
	// between the two consumers. A deletion cannot be REPLAYED faithfully
	// through an NFS mount, which is what partial is about; it can be
	// applied to a cache exactly (ADR 0044), where a stale entry would
	// otherwise shadow a file that is gone.
	w.observe(notify.Event{Export: root.export, Path: rel, Op: op, Dir: isDir})

	if root.mode == workspace.WatchPartial {
		// Strip what this mode will not misrepresent. If nothing is left, the
		// event is not worth a frame.
		op &^= notify.OpRemove | notify.OpRename
		if op == 0 {
			return
		}
	}

	c.add(time.Now(), notify.Event{Export: root.export, Path: rel, Op: op, Dir: isDir})
}

func opFor(e fsnotify.Event) notify.Op {
	var op notify.Op
	if e.Has(fsnotify.Create) {
		op |= notify.OpCreate
	}
	if e.Has(fsnotify.Write) {
		op |= notify.OpWrite
	}
	if e.Has(fsnotify.Remove) {
		op |= notify.OpRemove
	}
	if e.Has(fsnotify.Rename) {
		op |= notify.OpRename
	}
	if e.Has(fsnotify.Chmod) {
		op |= notify.OpAttrib
	}
	return op
}

// emit queues a frame, dropping rather than blocking.
func (w *Watcher) emit(out chan<- notify.Frame, events []notify.Event, notices []notify.Notice) {
	for _, n := range notices {
		w.lost(n)
		w.queue(out, notify.Frame{Notice: &n})
	}
	if len(events) == 0 {
		return
	}
	// Anything malformed is a bug on this side, and shipping it would have
	// the agent refuse the whole frame. Drop the event, say so, keep the rest.
	kept := events[:0]
	for _, e := range events {
		if err := e.Validate(); err != nil {
			w.log().Warn("not reporting a change", "err", err)
			w.countDropped(1)
			continue
		}
		kept = append(kept, e)
	}
	if len(kept) > 0 {
		w.queue(out, notify.Frame{Events: kept})
	}
}

func (w *Watcher) queue(out chan<- notify.Frame, frame notify.Frame) {
	select {
	case out <- frame:
	default:
		w.countDropped(uint64(max(len(frame.Events), 1)))
	}
}

// send is the only goroutine that touches the sink, so a stalled SSH channel
// back-pressures into the bounded queue rather than into the kernel.
func (w *Watcher) send(out <-chan notify.Frame) {
	for {
		select {
		case <-w.ctx.Done():
			return
		case frame, ok := <-out:
			if !ok {
				return
			}
			w.mu.Lock()
			sink := w.sink
			if sink == nil {
				for _, e := range frame.Events {
					w.disconnected[e.Export]++
				}
				w.stats.Dropped += uint64(len(frame.Events))
				w.mu.Unlock()
				continue
			}
			pending := w.takeDisconnected()
			w.mu.Unlock()

			// Whatever was lost while there was no connection is announced
			// before anything else, so the agent never mistakes a partial
			// picture for a complete one.
			for _, n := range pending {
				w.lost(n)
				if err := sink.Send(w.ctx, notify.Frame{Notice: &n}); err != nil {
					w.log().Warn("reporting dropped changes", "err", err)
				}
			}
			if err := sink.Send(w.ctx, frame); err != nil {
				w.log().Warn("sending changes", "err", err)
				w.countDropped(uint64(len(frame.Events)))
				continue
			}
			w.mu.Lock()
			w.stats.Sent += uint64(len(frame.Events))
			w.mu.Unlock()
		}
	}
}

// takeDisconnected drains the per-export tally accumulated with no sink. The
// caller holds w.mu.
func (w *Watcher) takeDisconnected() []notify.Notice {
	if len(w.disconnected) == 0 {
		return nil
	}
	notices := make([]notify.Notice, 0, len(w.disconnected))
	for export, n := range w.disconnected {
		notices = append(notices, notify.Notice{
			Export: export, Path: "/", Dropped: n, Reason: "disconnected",
		})
	}
	clear(w.disconnected)
	return notices
}

func (w *Watcher) publishTree(t *tree, c *coalescer) {
	dirs := make([]string, 0, len(t.denied))
	for d := range t.denied {
		dirs = append(dirs, d)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stats.Watched = len(t.dirs)
	w.stats.Denied = t.deniedN
	w.stats.DeniedDirs = dirs
	w.stats.Excluded = t.excluded
	w.stats.Pending = c.pendingCount()
}

func (w *Watcher) countDropped(n uint64) {
	w.mu.Lock()
	w.stats.Dropped += n
	w.mu.Unlock()
}

// log is the watcher's logger, or silence. See logx.Or.
func (w *Watcher) log() *slog.Logger {
	return logx.Or(w.opts.Log)
}
