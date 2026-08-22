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
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/lhns/remote-docker/core/logx"
	"github.com/lhns/remote-docker/core/workspace"
)

// Mode is how much of what we observe is worth sending.
type Mode int

const (
	// ModeOff watches nothing and opens no channel.
	ModeOff Mode = iota

	// ModePartial sends only what the agent can replay faithfully: writes and
	// creations. A deletion is never misrepresented as something else.
	ModePartial

	// ModeCoarse also sends deletions and renames, which the agent turns into
	// a poke at the parent directory. A watcher that rescans notices; one that
	// trusts the event kind is told something untrue. That is the user's trade
	// to make, which is why it is a setting and not a heuristic.
	ModeCoarse
)

func (m Mode) String() string {
	switch m {
	case ModeOff:
		return "off"
	case ModePartial:
		return "partial"
	case ModeCoarse:
		return "coarse"
	}
	return fmt.Sprintf("Mode(%d)", int(m))
}

// ParseMode reads the configured value. Off is the default, including for the
// empty string, so an unset variable disables the feature rather than enabling
// something surprising.
func ParseMode(s string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "off", "false", "0", "no":
		return ModeOff, nil
	case "partial", "on", "true", "1", "yes":
		return ModePartial, nil
	case "coarse", "best-effort":
		return ModeCoarse, nil
	}
	return ModeOff, fmt.Errorf("fswatch: unknown watch mode %q; want off, partial or coarse", s)
}

// Share is one export and the local directory behind it.
//
// Deliberately not *nfsserve.Share: the same decoupling as rewrite.Sharer, so
// this package does not import the NFS server in order to be tested.
type Share struct {
	ExportPath string
	LocalPath  string

	// File is the base name when this share exports a single file rather than
	// a directory (ADR 0039). The watch then goes on the CONTAINING directory,
	// because that is where the events for a file arrive -- an editor writing
	// through a temporary file replaces the inode, and a watch on the old one
	// sees nothing -- and everything but that name is dropped.
	File string
}

// Sink carries frames to the agent. Nil while no connection is established.
type Sink interface {
	Send(ctx context.Context, frame workspace.NotifyFrame) error
}

// DefaultQueueLen bounds the frames waiting to be sent. A full queue is an
// overflow, not a stall: blocking back into the event drain would cause a
// kernel-level IN_Q_OVERFLOW, which is both worse and less informative than
// saying so ourselves.
const DefaultQueueLen = 256

// Options configures a Watcher. The zero value of every field is a working
// default except Mode, which must be set for the watcher to do anything.
type Options struct {
	Mode     Mode
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

// Stats is what `status` reports. Everything that was dropped appears here,
// because a cap the user cannot see is a cap that lies.
type Stats struct {
	Mode       Mode
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
	goos string
	be   backend

	// raw decouples draining the backend from processing. The drain goroutine
	// does nothing but move events, so a slow walk in the processor cannot
	// back up into the kernel's own queue.
	raw   chan fsnotify.Event
	syncC chan []Share

	mu           sync.Mutex
	sink         Sink
	stats        Stats
	disconnected map[string]int

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	closed sync.Once
}

// New starts a watcher. A ModeOff watcher is returned ready and inert, so
// callers need no nil checks.
func New(opts Options) (*Watcher, error) {
	if opts.goos == "" {
		opts.goos = runtime.GOOS
	}
	if opts.Budget <= 0 {
		opts.Budget = DefaultBudget()
	}
	if opts.Exclude == nil {
		opts.Exclude = DefaultExcludes
	}
	if opts.QueueLen <= 0 {
		opts.QueueLen = DefaultQueueLen
	}
	if opts.newBackend == nil {
		opts.newBackend = newFsnotifyBackend
	}

	ctx, cancel := context.WithCancel(context.Background())
	w := &Watcher{
		opts:         opts,
		goos:         opts.goos,
		raw:          make(chan fsnotify.Event, 4096),
		syncC:        make(chan []Share, 1),
		disconnected: make(map[string]int),
		ctx:          ctx,
		cancel:       cancel,
	}
	w.stats.Mode = opts.Mode
	w.stats.Budget = opts.Budget

	if opts.Mode == ModeOff {
		return w, nil
	}

	be, err := opts.newBackend()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("fswatch: starting the file watcher: %w", err)
	}
	w.be = be

	out := make(chan workspace.NotifyFrame, opts.QueueLen)
	w.wg.Add(3)
	go func() { defer w.wg.Done(); w.drain() }()
	go func() { defer w.wg.Done(); w.process(out) }()
	go func() { defer w.wg.Done(); w.send(out) }()
	return w, nil
}

// Sync tells the watcher which shares exist now. Idempotent and diff-based, so
// calling it from both the registration path and a periodic reconcile costs
// nothing.
func (w *Watcher) Sync(shares []Share) {
	if w.opts.Mode == ModeOff {
		return
	}
	// Non-blocking with replacement: only the latest set matters, and a
	// caller reconciling on a ticker must never be held up by a walk.
	select {
	case w.syncC <- shares:
	default:
		select {
		case <-w.syncC:
		default:
		}
		select {
		case w.syncC <- shares:
		default:
		}
	}
}

// SetSink attaches a connection. Anything dropped while there was none is
// reported first, so the agent knows its picture starts incomplete.
func (w *Watcher) SetSink(s Sink) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sink = s
	w.stats.Connected = s != nil
}

// ClearSink detaches the connection without disturbing the watches.
func (w *Watcher) ClearSink() { w.SetSink(nil) }

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
			}
		case err, ok := <-w.be.Errors():
			if !ok {
				return
			}
			w.log().Warn("file watcher", "err", err)
		}
	}
}

// process owns the tree and the coalescer exclusively, so neither needs a lock.
func (w *Watcher) process(out chan<- workspace.NotifyFrame) {
	defer close(out)

	t := newTree(w.goos, w.be, w.opts.Budget, w.opts.Exclude, w.opts.Log)
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
	root, rel, ok := t.rootFor(e.Name)
	if !ok {
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
		if info, err := statNoFollow(e.Name); err == nil && info.IsDir() {
			isDir = true
			if !t.isExcluded(filepath.Base(e.Name)) {
				t.addTree(root, e.Name, func(p string, dir bool) {
					if childRel, ok := relativeTo(w.goos, root.parts, p); ok {
						c.add(time.Now(), workspace.FSEvent{
							Export: root.export, Path: childRel,
							Op: workspace.OpCreate, Dir: dir,
						})
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
	if w.opts.Mode == ModePartial {
		// Strip what this mode will not misrepresent. If nothing is left, the
		// event is not worth a frame.
		op &^= workspace.OpRemove | workspace.OpRename
		if op == 0 {
			return
		}
	}

	c.add(time.Now(), workspace.FSEvent{Export: root.export, Path: rel, Op: op, Dir: isDir})
}

func opFor(e fsnotify.Event) workspace.FSOp {
	var op workspace.FSOp
	if e.Has(fsnotify.Create) {
		op |= workspace.OpCreate
	}
	if e.Has(fsnotify.Write) {
		op |= workspace.OpWrite
	}
	if e.Has(fsnotify.Remove) {
		op |= workspace.OpRemove
	}
	if e.Has(fsnotify.Rename) {
		op |= workspace.OpRename
	}
	if e.Has(fsnotify.Chmod) {
		op |= workspace.OpAttrib
	}
	return op
}

// emit queues a frame, dropping rather than blocking.
func (w *Watcher) emit(out chan<- workspace.NotifyFrame, events []workspace.FSEvent, notices []workspace.FSNotice) {
	for _, n := range notices {
		w.queue(out, workspace.NotifyFrame{Notice: &n})
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
		w.queue(out, workspace.NotifyFrame{Events: kept})
	}
}

func (w *Watcher) queue(out chan<- workspace.NotifyFrame, frame workspace.NotifyFrame) {
	select {
	case out <- frame:
	default:
		w.countDropped(uint64(max(len(frame.Events), 1)))
	}
}

// send is the only goroutine that touches the sink, so a stalled SSH channel
// back-pressures into the bounded queue rather than into the kernel.
func (w *Watcher) send(out <-chan workspace.NotifyFrame) {
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
				if err := sink.Send(w.ctx, workspace.NotifyFrame{Notice: &n}); err != nil {
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
func (w *Watcher) takeDisconnected() []workspace.FSNotice {
	if len(w.disconnected) == 0 {
		return nil
	}
	notices := make([]workspace.FSNotice, 0, len(w.disconnected))
	for export, n := range w.disconnected {
		notices = append(notices, workspace.FSNotice{
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

// statNoFollow reports what a path is without following a final symlink. A
// symlink to a directory must not be walked into: inotify would watch the
// target, letting a watch escape the share that osfs.WithBoundOS() exists to
// bound.
func statNoFollow(p string) (os.FileInfo, error) { return os.Lstat(p) }
