package dircache

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lhns/remote-docker/core/cache"
	"github.com/lhns/remote-docker/core/logx"
)

// Store is where a cache physically lives.
//
// The share id is opaque: nothing here interprets it, and it is whatever the
// Store and its far end already agree on.
type Store interface {
	// Apply puts one batch of files, read from root on this machine, into a
	// share's cache. The Store reads them: it owns the encoding, and the
	// length it has to declare depends on it.
	Apply(ctx context.Context, share, root string, entries []Entry) error

	// Drop removes paths from a share's cache. Any number: a Store that has a
	// request size to respect splits internally.
	Drop(ctx context.Context, share string, paths []string) error

	// Changes reports what the consumer did to a share's cache since it was
	// filled. ErrShareGone means the cache no longer exists, which is a reason
	// to stop asking rather than to retry.
	Changes(ctx context.Context, share string) ([]cache.Change, error)

	// Pull fetches the named paths, calling into once per file. The reader is
	// valid only for that call.
	Pull(ctx context.Context, share string, paths []string, into func(File) error) error
}

// File is one file coming back out of a cache.
type File struct {
	// Path is share-relative with a leading slash, as every path here is.
	Path string

	// ModTime is when the consumer wrote it, which is what a plain mount would
	// have shown and what the next round compares against.
	ModTime time.Time

	Mode fs.FileMode
	Body io.Reader
}

// ErrShareGone is a Store saying it holds no cache for a share.
var ErrShareGone = errors.New("dircache: the store has no cache for this share")

// Record is what a previous fill put in a cache, surviving this process.
//
// The only thing that can tell a file deleted while nothing was running from a
// file the consumer created: a fill only ever writes, so it has no way to
// notice what is GONE. Optional; without one, nothing is ever removed from a
// cache and a share is stale in that one way.
type Record interface {
	Filled(share string) ([]string, bool)
	Record(share string, paths []string)
}

// Cache fills, invalidates and writes back the caches of the shares it is told
// about. Zero value is not usable; every field below is read as given.
type Cache struct {
	// Store resolves a Store per call, and returns false when there is none.
	//
	// Per call rather than held once: a fill outlives the request that started
	// it, and the connection underneath can be released and reopened while it
	// runs (ADR 0015).
	Store func() (Store, bool)

	// Record persists what each fill sent. Nil records nothing.
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

	// Ctx bounds the background work, because fills outlive the calls that
	// start them. quiet reads it too, to tell a shutdown from a fault.
	Ctx context.Context

	shares shares
	inval  invalidator
}

// Fill starts filling a share's cache from root, and returns at once.
//
// Started once per share: a second consumer of the same directory finds the
// fill already running or finished, and re-sending the tree would cost the link
// twice for nothing.
func (c *Cache) Fill(share, root string) {
	if _, running := c.shares.get(share); running {
		return
	}
	state := &fillState{}
	c.shares.set(share, root, state)

	go func() {
		// Before the fill, because a fill cannot remove anything.
		c.reconcileDeletions(share, root)

		err := c.fill(share, root, state)
		c.shares.mu.Lock()
		state.Done, state.Err = true, err
		// What the tree held against what the cache got, which is the question
		// write-back turns on. Not "did every batch we sent arrive", which is
		// the same number counted twice: a budget that stops the fill short
		// leaves batches that all succeeded and a cache that is missing files.
		state.Cached = err == nil && state.Stats.Complete()
		c.shares.mu.Unlock()

		if err != nil {
			// Not fatal to anything: the share works, it is just slower than
			// it would have been. Said once rather than per batch.
			c.quiet(c.Ctx, "a share's cache could not be filled", "share", share, "err", err)
		}

		// What the NEXT run has to reconcile against. Recorded even on a
		// partial fill, because it names what is in the cache, and what is in
		// the cache is what a later deletion has to be able to remove.
		if c.Record != nil {
			c.Record.Record(share, c.shares.paths(share))
		}
	}()
}

// Reports is one entry per share this cache holds, for a status command.
func (c *Cache) Reports() []Report { return c.shares.reports() }

// Stop cancels anything waiting to be sent. A pending invalidation runs on a
// timer, so it can fire after everything it needs has gone.
func (c *Cache) Stop() { c.inval.stop() }

// reconcileDeletions takes out of the cache what this machine no longer has,
// from what a previous run recorded.
func (c *Cache) reconcileDeletions(share, root string) {
	if c.Record == nil {
		return
	}
	filled, ok := c.Record.Filled(share)
	if !ok {
		return
	}
	c.dropDeleted(share, root, filled)
}

// dropDeleted removes from a share's cache the named paths this machine no
// longer has.
//
// Callers pass only paths a fill recorded: a path in the cache that no fill
// sent is the consumer's own file, and this must never remove one.
func (c *Cache) dropDeleted(share, root string, filled []string) {
	gone := deletedSince(root, filled)
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
	c.log().Info("took deleted files out of a share's cache", "share", share, "files", len(gone))
}

// fill scans the tree and uploads from it at the same time, cheapest first.
//
// The policy is stream's; this supplies what it cannot know: where the bytes go.
func (c *Cache) fill(share, root string, state *fillState) error {
	stats, err := stream(root, c.Exclude, c.Budget, func(batch []Entry) error {
		return c.sendBatch(share, root, batch, state)
	})

	c.shares.mu.Lock()
	state.Stats = stats
	c.shares.mu.Unlock()
	return err
}

// sendBatch applies one batch.
func (c *Cache) sendBatch(share, root string, entries []Entry, state *fillState) error {
	store, ok := c.Store()
	if !ok {
		// Nowhere to send it. Not an error: the share is served from the
		// authoritative tree meanwhile, and the next fill starts from scratch.
		return nil
	}

	ctx, cancel := context.WithTimeout(c.Ctx, fillBatchTimeout)
	defer cancel()
	if err := store.Apply(ctx, share, root, entries); err != nil {
		return err
	}

	c.shares.mu.Lock()
	state.Sent += len(entries)
	c.shares.mu.Unlock()
	c.shares.noteSent(share, root, entries)
	return nil
}

// fillBatchTimeout bounds one send. Generous, because a batch is up to
// batchBytes over whatever link the store is on, and a fill that gives up early
// leaves a share slower rather than broken.
const fillBatchTimeout = 10 * time.Minute

func (c *Cache) log() *slog.Logger { return logx.Or(c.Log) }

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

// deletedSince reports which of the recorded paths this machine no longer has.
//
// Local work only, one stat per path: the answer is about this machine's own
// disk, and asking the store could not improve it.
func deletedSince(root string, filled []string) []string {
	var gone []string
	for _, p := range filled {
		name := strings.TrimPrefix(p, "/")
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(name))); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				gone = append(gone, "/"+name)
			}
			// Any other error is this machine failing to answer about its own
			// file, which is not evidence that the file is gone.
		}
	}
	return gone
}

// fillState is what a share's fill has done so far, for reporting.
type fillState struct {
	Stats  Stats
	Sent   int
	Done   bool
	Err    error
	Cached bool // whether the cache holds everything the plan chose
}

// Report is what one share has cached, for a status command.
type Report struct {
	// Local is the directory on this machine behind the share.
	Local string

	// Stats is what the walk found: what will be cached, against what the tree
	// holds, so the two can be shown as a fraction.
	Stats Stats

	// Sent is how many files have actually gone into the cache, which while a
	// fill runs is the only number that is known.
	Sent int

	// Done says the fill has finished, and Err why it stopped if it did.
	Done bool
	Err  error
}

// shares is what this cache knows about each share it holds.
type shares struct {
	mu    sync.Mutex
	state map[string]*fillState

	// roots is where each share's files are on this machine, which is what
	// invalidation needs to read a changed file back.
	roots map[string]string

	// manifests are what the fill put in each cache: for every path, the size
	// and modification time it had HERE when it was sent. That is the baseline
	// write-back compares both sides against, which is what lets it decide
	// almost every case without comparing two machines' clocks (ADR 0044).
	manifests map[string]map[string]baseline
}

func (f *shares) set(share, root string, s *fillState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state == nil {
		f.state = map[string]*fillState{}
		f.roots = map[string]string{}
		f.manifests = map[string]map[string]baseline{}
	}
	f.state[share] = s
	f.roots[share] = root
	f.manifests[share] = map[string]baseline{}
}

// forget drops a share, so a later consumer of the same directory fills it from
// scratch rather than against a manifest for a cache that is gone.
func (f *shares) forget(share string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.state, share)
	delete(f.roots, share)
	delete(f.manifests, share)
}

// noteSent records what a batch put in a share's cache.
//
// Read from disk again rather than taken from the walk: what matters is the
// file as it was SENT, and one rewritten between the walk and the read would
// otherwise be recorded as something it never was, which write-back would later
// read as the consumer having changed it.
func (f *shares) noteSent(share, root string, entries []Entry) {
	f.mu.Lock()
	defer f.mu.Unlock()

	manifest := f.manifests[share]
	if manifest == nil {
		return
	}
	for _, e := range entries {
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(e.Path)))
		if err != nil {
			continue
		}
		manifest["/"+e.Path] = baseline{Size: info.Size(), ModTime: info.ModTime()}
	}
}

// paths is what this run put in a share's cache.
func (f *shares) paths(share string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]string, 0, len(f.manifests[share]))
	for p := range f.manifests[share] {
		out = append(out, p)
	}
	return out
}

// baselines is a COPY of what the fill sent for a share.
//
// Copied rather than handed out: the fill may still be adding to it while a
// round of write-back is deciding, and a map read under one lock and used under
// none is how a decision about somebody's files goes wrong at random.
func (f *shares) baselines(share string) map[string]baseline {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make(map[string]baseline, len(f.manifests[share]))
	for p, b := range f.manifests[share] {
		out[p] = b
	}
	return out
}

// rebase records what both sides now agree on.
//
// Without it the same change is decided again on the next round: the file here
// would still differ from what the fill sent, so a write-back would look like a
// conflict with itself.
func (f *shares) rebase(share, root string, actions []action) {
	f.mu.Lock()
	defer f.mu.Unlock()

	manifest := f.manifests[share]
	if manifest == nil {
		return
	}
	for _, a := range actions {
		name := strings.TrimPrefix(a.Path, "/")
		switch {
		case a.kind == kindDelete:
			delete(manifest, a.Path)
		case a.kind == kindWrite || (a.kind == kindConflict && a.Wins):
			info, err := os.Stat(filepath.Join(root, filepath.FromSlash(name)))
			if err != nil {
				delete(manifest, a.Path)
				continue
			}
			manifest[a.Path] = baseline{Size: info.Size(), ModTime: info.ModTime()}
		}
	}
}

// reports is one entry per share.
func (f *shares) reports() []Report {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]Report, 0, len(f.state))
	for share, state := range f.state {
		out = append(out, Report{
			Local: f.roots[share],
			Stats: state.Stats,
			Sent:  state.Sent,
			Done:  state.Done,
			Err:   state.Err,
		})
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

	out := make([]string, 0, len(f.roots))
	for share := range f.roots {
		out = append(out, share)
	}
	return out
}

func (f *shares) get(share string) (fillState, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.state[share]
	if !ok {
		return fillState{}, false
	}
	return *s, true
}
