package dircache

import (
	"cmp"
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Store is where a cache physically lives.
//
// The share id is opaque: nothing here interprets it, and it is whatever the
// Store and its far end already agree on.
type Store interface {
	// Apply puts one batch of files, read from root on this machine, into a
	// share's cache. The Store reads them: it owns the encoding, and the
	// length it has to declare depends on it.
	//
	// With fill, an entry the consumer has its own version of is left as it is
	// and named in Applied.Kept. The prefetch asks, because a batch is often set
	// off by the consumer's own copy-up of the file it is writing, and landing
	// it would lose that write (ADR 0044); invalidation does not, since an edit
	// here is decided by the change times before it is sent. A Store that
	// cannot tell applies the entry like any other.
	Apply(ctx context.Context, share, root string, entries []Entry, fill bool) (Applied, error)

	// Drop removes paths from a share's cache. Any number: a Store that has a
	// request size to respect splits internally.
	Drop(ctx context.Context, share string, paths []string) error

	// Changes reports what the consumer did to a share's cache since it was
	// filled. ErrShareGone means the cache no longer exists, which is a reason
	// to stop asking rather than to retry.
	Changes(ctx context.Context, share string) ([]Change, error)

	// Pull fetches the named paths, calling into once per file. The reader is
	// valid only for that call.
	Pull(ctx context.Context, share string, paths []string, into func(File) error) error
}

// Presenter is a Store that can also say which paths exist in the consumer's
// merged view of a share (what a container sees, the cache over the live export).
// Write-back asks it before deleting anything a removed or replaced directory
// implies; a Store without it, or whose answer fails, deletes nothing that way.
type Presenter interface {
	Present(ctx context.Context, share string, paths []string) ([]string, error)
}

// Applied is what one batch did.
type Applied struct {
	// Read is each file as the Store read it here. The baseline is recorded
	// from it and never from a later stat, which may describe a version
	// written since: the copy that landed would then read as the consumer's
	// change (ADR 0044). A file the Store could not read is not in it.
	Read []Read

	// Skipped names the entries the far end could not write and left out,
	// where the rest of the batch landed. A Store whose far end fails the
	// whole batch returns an error instead.
	Skipped []string

	// Kept names the entries left as the consumer had them, under fill.
	Kept []string
}

// Read is one file as a Store read it: the size and modification time it had
// here at that moment. Path is as in Entry.
type Read struct {
	Path    string
	Size    int64
	ModTime time.Time
}

// ErrCannotPresent is a Presenter whose far end cannot answer at all, such as an
// older consumer. Nothing is deleted on a directory's say-so, and nothing is
// said about it every round.
var ErrCannotPresent = errors.New("dircache: the store cannot say which paths exist")

// File is one file coming back out of a cache.
type File struct {
	// Path is share-relative with a leading slash, as every path here is.
	Path string

	// ModTime is when the consumer wrote it, which is what a plain mount would
	// have shown and what the next round compares against.
	ModTime time.Time

	Mode fs.FileMode
	Body io.Reader

	// Unreadable, with no Body, is a path the store could not read and names
	// instead of failing the files beside it: why, as a person reads it.
	Unreadable string
}

// ErrShareGone is a Store saying it holds no cache for a share.
var ErrShareGone = errors.New("dircache: the store has no cache for this share")

// Record is every path this machine put in a cache, and what each looked like
// when it went, surviving this process.
//
// The paths are the only thing that can tell a file deleted while nothing was
// running from a file the consumer created: a fill only ever writes, so it has
// no way to notice what is GONE. Saved for every batch, whatever sent it: one
// that lands unrecorded is a deletion nothing can reconcile.
//
// The baselines are what write-back compares both sides against, so after a
// restart a file the consumer modified is still told from one this machine made
// (ADR 0027, ADR 0044). A path with a zero Baseline was recorded before its
// batch landed and has none. Optional; without one, nothing is ever removed
// from a cache and a restarted share carries back only files it never held.
type Record interface {
	Filled(share string) (map[string]Baseline, bool)
	Record(share string, filled map[string]Baseline)
}

// Cache prefetches, invalidates and writes back the caches of the shares it is
// told about. Zero value is not usable; every field below is read as given.
type Cache struct {
	// Store resolves a Store per call, and returns false when there is none.
	//
	// Per call rather than held once: a prefetch outlives the request that
	// started it, and the connection underneath can be released and reopened
	// while it runs (ADR 0015).
	Store func() (Store, bool)

	// Record persists what every batch sent. Nil records nothing.
	Record Record

	// Exclude are directory names never cached. It MUST be the list the change
	// source honours, and that is a rule rather than a convenience: a cache may
	// only hold what can be invalidated, or a file changed here stays stale for
	// the consumer until something else removes it.
	Exclude []string

	// Budget bounds what one share copies across.
	Budget Budget

	// Skew is the far end's clock minus this machine's. Used for one
	// comparison only, which side wrote last when both changed a file. Nil is
	// no skew.
	Skew func() time.Duration

	// Log is where anything worth a person's attention goes. Nil discards.
	Log *slog.Logger

	// Ctx bounds the background work, because prefetches outlive the calls
	// that start them. quiet reads it too, to tell a shutdown from a fault.
	Ctx context.Context

	// Link supplies the round trip time, asked per attach and per batch. The
	// bandwidth in what it returns is ignored: this cache measures its own on
	// the batches it sends (link). Nil is an unmeasured RTT, and the tree then
	// falls back to its leaf size.
	Link func() Link

	// ChangeTime is when a path in the share last changed on this machine: its
	// ctime, which every write, rename and change of mode moves and no tool
	// sets back. Last writer wins reads it for a file the cache never held
	// (decide). Supplied rather than read here because it is a different
	// structure on every platform; false, or a nil func, is not known, and a
	// time that is not known never writes or deletes anything.
	ChangeTime func(r *os.Root, rel string) (time.Time, bool)

	shares shares
	inval  invalidator

	// rounds keeps write-back rounds one at a time: the per-connection loop
	// and the round Settle runs when the session closes. A channel rather than
	// a mutex, so waiting for it honours the caller's context: the loop may be
	// mid-round on a context nobody cancels until Settle has returned.
	roundsOnce sync.Once
	rounds     chan struct{}

	// bw is the bandwidth measured on this cache's own batches, in bytes per
	// second, and zero until a batch large enough has gone.
	bw atomic.Int64

	// recording keeps saves of the record in the order they were made.
	recording sync.Mutex

	// changed is what the consumer was last asked it had changed, per share, for
	// a few seconds (consumerChanged). One entry per share id this process has
	// filled, so it is left when a share is forgotten.
	changedMu sync.Mutex
	changed   map[string]changedAnswer

	// The rest are seams for tests: how long one call to the store may take,
	// how one file and one link land here, how a directory here is listed for
	// a case clash, the clock write-back backs off by, and the first wait after
	// a failed fill batch. Zero and nil mean writeBackTimeout, writeUnder,
	// landLink, readDirNames, time.Now and fillBackoffMin.
	callBudget  time.Duration
	writeFile   func(r *os.Root, f File) error
	makeLink    func(r *os.Root, rel, target string) error
	listDir     func(r *os.Root, dir string) ([]string, error)
	now         func() time.Time
	fillBackoff time.Duration
}

// clock is the time write-back backs off by.
func (c *Cache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// ShareOptions is what the caller decided about a share, in this module's
// terms rather than the caller's: how to fill its cache ahead of reads, and
// whether what the consumer writes ever comes back.
type ShareOptions struct {
	// Policy is how this share's cache is filled ahead of reads: the policy is
	// the share's, so one cache serves shares that disagree. Off or zero, the
	// share is attached for invalidation and write-back only, and its cache
	// holds what the consumer wrote and nothing else.
	Policy Policy

	// Ephemeral discards the consumer's writes: nothing is ever carried back,
	// including when the share goes, and its changes are never even asked
	// for. Invalidation from this machine still applies; ephemeral is about
	// the direction out, not coherence in.
	Ephemeral bool

	// Exec is the execute bits the consumer sees as this machine's file has
	// them: 0111 less what the share's mask clears. Zero where this machine
	// has no execute bit to carry (Windows, whose share reports every file
	// executable), which turns comparing and carrying modes off.
	Exec fs.FileMode
}

// Attach registers a share and, if asked, starts filling its cache from root.
// Returns at once either way.
//
// Attached once per share: a second consumer of the same directory finds the
// share already known, and re-sending the tree would cost the link twice for
// nothing. It can still ask for more: a stronger policy raises the share's,
// and a weaker one is ignored (ADR 0042).
func (c *Cache) Attach(share, root string, opts ShareOptions) {
	if _, known := c.shares.get(share); known {
		c.raise(share, root, opts.Policy)
		return
	}
	state := &shareState{opts: opts}
	var filled map[string]Baseline
	if c.Record != nil {
		filled, _ = c.Record.Filled(share)
	}
	c.shares.set(share, root, state, filled)
	// Whatever the policy: a cache filled by an earlier session still holds a
	// file deleted here while nothing ran, and only the record of that fill
	// can take it out.
	go c.reconcileDeletions(share, root)
	if opts.Policy == PolicyOff || opts.Policy == "" {
		// Nothing to prefetch: the cache holds only what the consumer writes,
		// and status calls a share that chose nothing complete.
		c.shares.finish(state, true, nil)
		return
	}
	c.startTree(share, root, state, opts.Policy)
}

// Reports is one entry per share this cache holds, for a status command.
func (c *Cache) Reports() []Report { return c.shares.reports() }

// Stop sends what is waiting to be sent and cancels the timer. Sent rather than
// dropped: an edit made here a moment before the session closes is otherwise
// missing from the cache for as long as the container runs. Bounded, because a
// workspace that has stopped answering must not hold the close.
func (c *Cache) Stop() {
	ctx, cancel := context.WithTimeout(cmp.Or(c.Ctx, context.Background()), stopFlushTimeout)
	defer cancel()
	c.flushNow(ctx)
	c.inval.stop()
}

// stopFlushTimeout bounds what Stop sends. Settle, which a closing session
// runs first, has already sent the same queue within its own bound.
const stopFlushTimeout = 15 * time.Second

// reconcileDeletions takes out of the cache the recorded paths this machine no
// longer has. Only recorded ones: a path no batch sent is the consumer's own.
func (c *Cache) reconcileDeletions(share, root string) {
	gone := deletedSince(root, c.shares.recordedPaths(share))
	if len(gone) == 0 {
		return
	}

	store, ok := c.Store()
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(c.Ctx, invalidateTimeout)
	defer cancel()

	if err := store.Drop(ctx, share, gone); err != nil {
		c.quiet(ctx, "removing from a cache what this machine no longer has",
			"share", share, "err", err)
		return
	}
	c.unrecord(share, gone)
	c.log().Info("took deleted files out of a share's cache", "share", share, "files", len(gone))
}

// sendBatch applies one batch.
func (c *Cache) sendBatch(share, root string, entries []Entry, state *shareState) error {
	store, ok := c.Store()
	if !ok {
		// Nowhere to send it. Not an error: the share is served from the
		// authoritative tree meanwhile, and the next prefetch starts from
		// scratch.
		return nil
	}

	// Never over a version the consumer has not carried back. A resumed fill
	// re-sends a file changed here while nothing ran, and if the consumer changed
	// it too, sending would overwrite the consumer's version and reset the
	// baseline, so the both-changed rule would never run. If the consumer cannot
	// be asked the batch fails and is retried, since sending blind is the loss.
	if sendable, err := c.withoutConsumerChanges(share, root, store, entries); err != nil {
		return err
	} else if len(sendable) != len(entries) {
		entries = sendable
		if len(entries) == 0 {
			return nil
		}
	}

	var bytes int64
	for _, e := range entries {
		bytes += e.Size
	}

	// Bytes count when a batch is sent and files when it has landed, so
	// status shows an apply in flight as carried.
	c.shares.mu.Lock()
	state.Bytes += bytes
	c.shares.mu.Unlock()

	c.record(share, entries)

	ctx, cancel := context.WithTimeout(c.Ctx, batchTimeout)
	defer cancel()
	started := time.Now()
	applied, err := store.Apply(ctx, share, root, entries, true)
	if err != nil {
		return err
	}
	c.observeBandwidth(bytes, time.Since(started))

	if len(applied.Skipped) > 0 {
		c.log().Warn("the workspace could not take some files into a share's cache; they are read live",
			"share", share, "files", len(applied.Skipped), "first", strings.TrimPrefix(applied.Skipped[0], "/"))
	}
	c.shares.mu.Lock()
	state.Sent += len(withoutSkipped(entries, slices.Concat(applied.Skipped, applied.Kept)))
	c.shares.mu.Unlock()
	c.noteApplied(share, applied)
	return nil
}

// noteApplied records what a batch put in a share's cache. What the far end
// left out is not in the cache, and what it kept is the consumer's change,
// which write-back decides: neither is recorded or compared against. The rest
// has the baseline of the version the Store read.
func (c *Cache) noteApplied(share string, applied Applied) {
	read := applied.Read
	if notOurs := slices.Concat(applied.Skipped, applied.Kept); len(notOurs) > 0 {
		c.unrecord(share, notOurs)
		left := make(map[string]bool, len(notOurs))
		for _, p := range notOurs {
			left[slashed(p)] = true
		}
		read = slices.DeleteFunc(slices.Clone(read), func(r Read) bool { return left[slashed(r.Path)] })
	}
	c.shares.noteSent(share, read)
	c.saveRecord(share)
}

// changedAnswer is one answer to what the consumer changed.
type changedAnswer struct {
	at      time.Time
	changes map[string]Change
}

// consumerChangedTTL is how long an answer is reused: a resumed fill sends
// batches back to back, and each answer is a walk of the consumer's layer.
const consumerChangedTTL = 2 * time.Second

// consumerChanged is what the consumer reports, by path. fresh asks again
// whatever was asked a moment ago: an invalidation decides who wrote last, and
// a container write in the last two seconds is exactly the one that matters.
func (c *Cache) consumerChanged(ctx context.Context, share string, store Store, fresh bool) (map[string]Change, error) {
	c.changedMu.Lock()
	cached, ok := c.changed[share]
	c.changedMu.Unlock()
	if !fresh && ok && c.clock().Sub(cached.at) < consumerChangedTTL {
		return cached.changes, nil
	}
	ctx, cancel := context.WithTimeout(ctx, c.budget())
	defer cancel()
	changes, err := store.Changes(ctx, share)
	if err != nil {
		return nil, err
	}
	return c.rememberChanges(share, changes), nil
}

// rememberChanges keeps an answer to what the consumer changed, by path, for
// the next consumerChanged within the TTL. Write-back's own round keeps its
// answer too, so a fill running beside it rarely asks again.
func (c *Cache) rememberChanges(share string, changes []Change) map[string]Change {
	byPath := make(map[string]Change, len(changes))
	for _, ch := range changes {
		byPath[ch.Path] = ch
	}
	c.changedMu.Lock()
	if c.changed == nil {
		c.changed = map[string]changedAnswer{}
	}
	c.changed[share] = changedAnswer{at: c.clock(), changes: byPath}
	c.changedMu.Unlock()
	return byPath
}

// withoutConsumerChanges is entries less the consumer's own changes to them,
// each said once as a conflict. The fill must never overwrite one: write-back
// decides which side is kept (ADR 0044).
func (c *Cache) withoutConsumerChanges(share, root string, store Store, entries []Entry) ([]Entry, error) {
	theirs, err := c.consumersOwn(c.Ctx, share, root, store, entries, false)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if _, t := theirs[slashed(e.Path)]; t {
			c.sayOnce(share, slashed(e.Path), "changed in the container and here before write-back carried it",
				"a file changed in the container and here; this machine's version is not sent over the container's, and write-back decides which is kept")
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// consumersOwn is what the consumer changed among entries, by path: a listed
// file that is not the copy this machine has or sent (fillsOwn). A copy of a
// file the cache never held (a chown -R) has the size and time of the file
// here, and sending over it loses nothing. fresh is consumerChanged's.
//
// Nothing for an ephemeral share: nothing is carried back from one, so there is
// no rule to protect, and the edit here is the version the container should get.
func (c *Cache) consumersOwn(ctx context.Context, share, root string, store Store, entries []Entry, fresh bool) (map[string]Change, error) {
	if len(entries) == 0 || c.shares.ephemeral(share) {
		return nil, nil
	}
	changed, err := c.consumerChanged(ctx, share, store, fresh)
	if err != nil || len(changed) == 0 {
		return nil, err
	}
	sent := c.shares.sentOf(share, entries)
	out := map[string]Change{}
	for _, e := range entries {
		p := slashed(e.Path)
		if ch, listed := changed[p]; listed && !ch.Opaque && !ch.Dir && !c.fillsOwn(root, p, ch, sent) {
			out[p] = ch
		}
	}
	return out, nil
}

// sayOnce logs msg about a path the first time it is said for this reason
// (writeBacks.conflictNew).
func (c *Cache) sayOnce(share, p, why, msg string, args ...any) {
	if c.shares.wb.conflictNew(share, p, why) {
		c.log().Warn(msg, append([]any{"path", strings.TrimPrefix(p, "/")}, args...)...)
	}
}

// fillsOwn reports whether sending this machine's file over a listed change
// loses nothing of the consumer's: the change is a copy of what this machine has
// or sent, or a deletion older than the file here. The last is the whiteout
// this machine's own deletion left (an invalidation drops through the union)
// for a file made here again since; kept out of the fill, the file would never
// reach the consumer. A deletion with no time goes as it did before times
// existed: sent over for a file the cache never held, not for one it did.
func (c *Cache) fillsOwn(root, p string, ch Change, sent map[string]Baseline) bool {
	base, held := sent[p]
	switch {
	case ch.Link != "":
		return false
	case ch.Deleted && ch.Changed == 0:
		return !held
	case ch.Deleted:
		ours, ok := c.localChangeTime(root, p)
		return ok && !consumerLater(ch.Changed, ours, c.skew(), c.margin())
	case held:
		return base.carries(ch)
	}
	info, err := os.Stat(localPath(root, p))
	return err == nil && sameContent(ch, info)
}

// localChangeTime is ChangeTime for one share path under root, false when it
// cannot be read.
func (c *Cache) localChangeTime(root, p string) (time.Time, bool) {
	if c.ChangeTime == nil {
		return time.Time{}, false
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return time.Time{}, false
	}
	defer func() { _ = r.Close() }()
	return c.ChangeTime(r, relPath(p))
}

// record saves a batch's paths BEFORE it is applied: a batch cut off midway may
// have landed in part, and a path recorded that never landed costs one Drop of
// nothing.
func (c *Cache) record(share string, entries []Entry) {
	paths := make([]string, len(entries))
	for i, e := range entries {
		paths[i] = e.Path
	}
	c.editRecord(share, paths, true)
}

// unrecord forgets paths the cache no longer holds, so a consumer that later
// creates one is not mistaken for the fill.
func (c *Cache) unrecord(share string, paths []string) { c.editRecord(share, paths, false) }

func (c *Cache) editRecord(share string, paths []string, add bool) {
	c.recording.Lock()
	defer c.recording.Unlock()
	if changed := c.shares.editRecord(share, paths, add); changed && c.Record != nil {
		c.Record.Record(share, c.shares.snapshot(share))
	}
}

// saveRecord saves a share's record as it stands, for a change to the
// baselines alone.
func (c *Cache) saveRecord(share string) {
	if c.Record == nil {
		return
	}
	c.recording.Lock()
	defer c.recording.Unlock()
	c.Record.Record(share, c.shares.snapshot(share))
}

// batchTimeout bounds one send: up to batchBytes over whatever link the store
// is on, and giving up early leaves a share slower rather than broken.
const batchTimeout = 10 * time.Minute

// log is the logger, or one that discards, so nil is not a case at every use.
func (c *Cache) log() *slog.Logger {
	if c.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return c.Log
}

// quiet reports an error unless the work it belonged to was already being torn
// down.
//
// A Store is usually one connection, so tearing that down makes every goroutine
// still using it fail at once with EOF or a half-read stream. Those describe a
// shutdown rather than anything wrong, and printing them after a one-shot
// command finished is how a clean exit comes to look like a crash.
func (c *Cache) quiet(ctx context.Context, msg string, args ...any) {
	if ctx.Err() != nil || (c.Ctx != nil && c.Ctx.Err() != nil) {
		return
	}
	c.log().Warn(msg, args...)
}

// localPath is where a share-relative path (leading slash or not) is on this
// machine.
func localPath(root, p string) string {
	return filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(p, "/")))
}

// deletedSince reports which of the recorded paths this machine no longer has.
//
// Local work only, one stat per path: the answer is about this machine's own
// disk, and asking the store could not improve it.
func deletedSince(root string, filled []string) []string {
	var gone []string
	for _, p := range filled {
		if _, err := os.Stat(localPath(root, p)); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				gone = append(gone, "/"+strings.TrimPrefix(p, "/"))
			}
			// Any other error is this machine failing to answer about its own
			// file, which is not evidence that the file is gone.
		}
	}
	return gone
}

// shareState is what a share's prefetch has done so far.
type shareState struct {
	Report

	// Cached is whether the cache holds everything the prefetch chose.
	Cached bool

	// opts is what the share was attached with.
	opts ShareOptions
}

// Report is what one share has cached, for a status command.
type Report struct {
	// Local is the directory on this machine behind the share.
	Local string

	// Stats is what the walk found: what will be cached, against what the tree
	// holds, so the two can be shown as a fraction.
	Stats Stats

	// Sent is how many files have actually gone into the cache, which while a
	// prefetch runs is the only number that is known, and Bytes how much. A
	// cache whose size nobody can see is a cache nobody can trust.
	Sent  int
	Bytes int64

	// Done says the prefetch has finished, and Err why it stopped if it did.
	Done bool
	Err  error

	// FillErr is why the last batch failed while the fill is being retried,
	// and FillRetry when it will be. Both are zero once a batch goes through.
	FillErr   error
	FillRetry time.Time

	// WriteBack is the state of carrying the consumer's writes back, and
	// Ephemeral says there is none: the share discards what the consumer writes.
	WriteBack WriteBackReport
	Ephemeral bool
}

// shares is what this cache knows about each share it holds.
type shares struct {
	mu    sync.Mutex
	state map[string]*shareState

	// prefetch is each share's tree and sender, for the eager and tree
	// policies.
	prefetch map[string]*prefetch

	// trees counts the prefetches whose policy is the tree. Touch runs on
	// every NFS read, and with no such share it must cost one atomic load and
	// not a lock.
	trees atomic.Int32

	// roots is where each share's files are on this machine, which is what
	// invalidation needs to read a changed file back.
	roots map[string]string

	// manifests are what the prefetch put in each cache: for every path, the
	// size and modification time it had HERE when it was sent. That is the
	// baseline write-back compares both sides against, which is what lets it
	// decide almost every case without comparing two machines' clocks (ADR
	// 0044).
	manifests map[string]map[string]Baseline

	// recorded is what Record holds for each share: every path any batch sent,
	// this run or an earlier one, less what has been dropped since.
	recorded map[string]map[string]bool

	// wb is what write-back remembers between rounds, under its own lock
	// (wbstate.go).
	wb writeBacks
}

// ephemeral reports whether a share never carries writes back.
func (f *shares) ephemeral(share string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.state[share]
	return ok && s.opts.Ephemeral
}

// exec is the execute bits a share's consumer sees as they are here
// (ShareOptions.Exec).
func (f *shares) exec(share string) fs.FileMode {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.state[share]; ok {
		return s.opts.Exec
	}
	return 0
}

func (f *shares) set(share, root string, s *shareState, filled map[string]Baseline) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state == nil {
		f.state = map[string]*shareState{}
		f.roots = map[string]string{}
		f.manifests = map[string]map[string]Baseline{}
		f.recorded = map[string]map[string]bool{}
	}
	s.Local = root
	f.state[share] = s
	f.roots[share] = root
	f.manifests[share] = map[string]Baseline{}
	f.recorded[share] = map[string]bool{}
	f.wb.reset(share)
	for p, b := range filled {
		p = slashed(p)
		f.recorded[share][p] = true
		if !b.ModTime.IsZero() {
			f.manifests[share][p] = b
		}
	}
}

// editRecord adds paths to a share's record, or removes them, and reports
// whether it changed. A path taken out loses its baseline too: it is not in the
// cache any more, so there is nothing to compare.
func (f *shares) editRecord(share string, paths []string, add bool) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	recorded, ok := f.recorded[share]
	if !ok {
		return false
	}
	changed := false
	for _, p := range paths {
		p = slashed(p)
		if recorded[p] != add {
			changed = true
			if add {
				recorded[p] = true
			} else {
				delete(recorded, p)
				delete(f.manifests[share], p)
			}
		}
	}
	return changed
}

// snapshot is a share's record with the baselines it has, for saving.
func (f *shares) snapshot(share string) map[string]Baseline {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]Baseline, len(f.recorded[share]))
	for p := range f.recorded[share] {
		out[p] = f.manifests[share][p]
	}
	return out
}

// recordedPaths is a share's record.
func (f *shares) recordedPaths(share string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return keys(f.recorded[share])
}

// recordedUnder is the paths given, and every recorded path under one of them:
// a directory dropped from the cache takes its files out of the record too.
func (f *shares) recordedUnder(share string, paths []string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := slices.Clone(paths)
	for p := range f.recorded[share] {
		for _, d := range paths {
			if strings.HasPrefix(p, strings.TrimSuffix(d, "/")+"/") {
				out = append(out, p)
				break
			}
		}
	}
	return out
}

// recordedSet is a COPY of a share's record, for a write-back round to decide
// against while batches go on adding to it.
func (f *shares) recordedSet(share string) map[string]bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]bool, len(f.recorded[share]))
	for p := range f.recorded[share] {
		out[p] = true
	}
	return out
}

func slashed(p string) string { return "/" + strings.TrimPrefix(p, "/") }

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// finish marks a share's prefetch over, with whether the cache holds
// everything it chose and why it stopped if it failed. The first call wins
// and reports true.
func (f *shares) finish(s *shareState, cached bool, err error) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s.Done {
		return false
	}
	s.Done, s.Cached, s.Err = true, cached, err
	return true
}

// forget drops a share, so a later consumer of the same directory fills it from
// scratch rather than against a manifest for a cache that is gone.
//
// The prefetch goes with it. Left behind, a Touch on a share attached again
// without prefetch would still find that tree and queue batches into it, so
// files would be sent to a share nobody asked to fill. Removing it from the
// map is also how the sender is told to stop, which it checks per round.
func (f *shares) forget(share string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.state, share)
	delete(f.roots, share)
	delete(f.manifests, share)
	delete(f.recorded, share)
	f.wb.drop(share)
	if p, ok := f.prefetch[share]; ok {
		delete(f.prefetch, share)
		if p.pol() == PolicyTree {
			f.trees.Add(-1)
		}
	}
}

// noteSent records the baseline of each file a batch landed, as the Store read
// it (Applied.Read).
func (f *shares) noteSent(share string, read []Read) {
	f.mu.Lock()
	defer f.mu.Unlock()

	manifest := f.manifests[share]
	if manifest == nil {
		return
	}
	for _, r := range read {
		p := slashed(r.Path)
		b := Baseline{Size: r.Size, ModTime: r.ModTime}
		// A file carried back from the consumer and then sent again
		// unchanged (the watcher sees the landing as a local change) is
		// still the consumer's.
		if old, had := manifest[p]; had && old.Consumer != 0 && old.sameFile(b) {
			b.Consumer = old.Consumer
		}
		manifest[p] = b
	}
}

// baselineOf is a file as this machine has it now.
func baselineOf(root, p string) (Baseline, bool) {
	info, err := os.Stat(localPath(root, p))
	if err != nil {
		return Baseline{}, false
	}
	return Baseline{Size: info.Size(), ModTime: info.ModTime()}, true
}

// alreadyHeld names the entries the cache holds from an earlier session: the
// record has a baseline for each and the file here still matches it. A fill that
// resumed would otherwise send the whole tree again over whatever a container
// has written since, and a changed file still goes, as a change here does.
func (f *shares) alreadyHeld(share, root string, entries []Entry) []string {
	manifest := f.sentOf(share, entries)
	var held []string
	for p, b := range manifest {
		rel := strings.TrimPrefix(p, "/")
		if now, ok := baselineOf(root, rel); ok && now.sameFile(b) {
			held = append(held, rel)
		}
	}
	return held
}

// sentOf is the baseline of each entry some batch already put in the cache, by
// path with a leading slash.
func (f *shares) sentOf(share string, entries []Entry) map[string]Baseline {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]Baseline{}
	for _, e := range entries {
		if b, ok := f.manifests[share][slashed(e.Path)]; ok {
			out[slashed(e.Path)] = b
		}
	}
	return out
}

// baselines is a COPY of a share's manifest: the prefetch may still be adding
// to it while a write-back round decides against it.
func (f *shares) baselines(share string) map[string]Baseline {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make(map[string]Baseline, len(f.manifests[share]))
	for p, b := range f.manifests[share] {
		out[p] = b
	}
	return out
}

// rebase records what both sides now agree on.
//
// Without it the same change is decided again on the next round: the file here
// would still differ from what the prefetch sent, so a write-back would look
// like a conflict with itself.
func (f *shares) rebase(share, root string, actions []action) {
	f.mu.Lock()
	defer f.mu.Unlock()

	manifest := f.manifests[share]
	if manifest == nil {
		return
	}
	for _, a := range actions {
		switch {
		case a.kind == kindDelete:
			delete(manifest, a.Path)
		case a.kind == kindLink:
			if info, err := os.Lstat(localPath(root, a.Path)); err == nil {
				manifest[a.Path] = linkBaseline(a.Link, info.ModTime())
			} else {
				delete(manifest, a.Path)
			}
		case a.kind == kindWrite || a.kind == kindAdopt || (a.kind == kindConflict && a.Wins):
			if b, ok := baselineOf(root, a.Path); ok {
				b.Consumer = a.Consumer
				manifest[a.Path] = b
			} else {
				delete(manifest, a.Path)
			}
		}
	}
}

// reports is one entry per share.
func (f *shares) reports() []Report {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]Report, 0, len(f.state))
	for share, state := range f.state {
		r := state.Report
		r.WriteBack = f.wb.report(share)
		r.Ephemeral = state.opts.Ephemeral
		out = append(out, r)
	}
	return out
}

// root is the local directory behind a share, and whether there is one.
func (f *shares) root(share string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	root, ok := f.roots[share]
	return root, ok
}

// all is every share this cache holds.
func (f *shares) all() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return keys(f.roots)
}

func (f *shares) get(share string) (shareState, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.state[share]
	if !ok {
		return shareState{}, false
	}
	return *s, true
}
