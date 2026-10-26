package dircache

import (
	"cmp"
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Keeping a cache honest when the tree under it changes (ADR 0044): a cached
// copy of a file changed here is stale until something removes it, which makes
// this the one way the mode can be wrong rather than slow.
//
//	changed here -> send the bytes again, overwriting the cached copy
//	deleted here -> remove it; the whiteout is right, the tree lost it too

// invalidateDelay batches the events of one editor save, one `git checkout` or
// one build step into a single exchange.
//
// Short enough that an edit reaches a consumer about as fast as a keystroke
// reaches a file, long enough that writing a thousand files is a handful of
// round trips rather than a thousand.
const invalidateDelay = 150 * time.Millisecond

// invalidateTimeout bounds one exchange. Shorter than a fill's, because this is
// what a person is waiting on: an edit that has not arrived is the thing they
// are watching for.
const invalidateTimeout = 2 * time.Minute

// invalidator is what has accumulated since the last exchange.
type invalidator struct {
	mu      sync.Mutex
	pending map[string]map[string]pend // share -> path -> what happened
	timer   *time.Timer
}

// pend is the last word about one path: whether it is gone, and whether it was
// a directory.
type pend struct{ gone, dir bool }

// Lost is called when the change source could not report everything it saw.
//
// A reconcile rather than a log line, and the difference matters: the events
// that were dropped may have been deletions, and a cached copy of a file that
// is gone shadows its absence for as long as it sits there. Every share this
// cache holds is checked against this machine's own disk, which is local work
// and no round trips unless something really is missing.
func (c *Cache) Lost(notice Notice) {
	c.log().Warn("the watcher dropped changes; checking the caches against this machine",
		"reason", notice.Reason, "dropped", notice.Dropped)

	for _, share := range c.shares.all() {
		root, ok := c.shares.root(share)
		if !ok {
			continue
		}
		go c.reconcileDeletions(share, root)
	}
}

// Observe is called for every change seen, for every share.
//
// Cheap and non-blocking on purpose: it runs on the change source's own path,
// and a share with no cache -- which is most of them -- costs one map lookup.
func (c *Cache) Observe(event Event) {
	if _, ok := c.shares.root(event.Share); !ok {
		return
	}
	if event.Dir && !event.Op.Gone() {
		// A directory is not cached in its own right: its files are, and each
		// of them arrives as its own event. Creating one changes nothing the
		// cache holds. Removing one does (Cache.invalidate).
		return
	}
	// Only what the cache may hold. A path under an excluded directory is
	// served live and has nothing to invalidate, and asking for it to be
	// dropped would be a round trip to say nothing.
	if excludedPath(event.Path, c.Exclude) {
		return
	}

	c.inval.mu.Lock()
	defer c.inval.mu.Unlock()
	if c.inval.pending == nil {
		c.inval.pending = map[string]map[string]pend{}
	}
	if c.inval.pending[event.Share] == nil {
		c.inval.pending[event.Share] = map[string]pend{}
	}
	// The last word about a path wins: a file written after being removed is a
	// write, and one removed after being written is a removal. Which ops count
	// as removal is Op.Gone.
	c.inval.pending[event.Share][event.Path] = pend{gone: event.Op.Gone(), dir: event.Dir}

	if c.inval.timer == nil {
		c.inval.timer = time.AfterFunc(invalidateDelay, c.flush)
	}
}

// stop cancels a pending flush and forgets what was waiting.
func (i *invalidator) stop() {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.timer != nil {
		i.timer.Stop()
		i.timer = nil
	}
	i.pending = nil
}

// take hands over what has accumulated, and cancels the timer that would have.
func (i *invalidator) take() map[string]map[string]pend {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.timer != nil {
		i.timer.Stop()
		i.timer = nil
	}
	pending := i.pending
	i.pending = nil
	return pending
}

// flush applies what has accumulated, when the timer fires.
func (c *Cache) flush() { c.flushNow(cmp.Or(c.Ctx, context.Background())) }

// flushNow applies what has accumulated, within ctx.
func (c *Cache) flushNow(ctx context.Context) {
	pending := c.inval.take()
	if c.Store == nil {
		return
	}
	for share, paths := range pending {
		root, ok := c.shares.root(share)
		if !ok {
			continue
		}
		c.invalidate(ctx, share, root, paths)
	}
}

// invalidate sends one share's changes: the removals as a drop, the rest as a
// batch of their current contents.
//
// A directory removed here is dropped as one, so the union hides it. Left, the
// empty skeleton a chown -R or the fill copied into the cache would be reported
// as directories the container made, made here again every round, and shown
// to the container, where a bind mount shows nothing.
func (c *Cache) invalidate(ctx context.Context, share, root string, paths map[string]pend) {
	store, ok := c.Store()
	if !ok {
		// Nowhere to send it. The cache is now behind, which the next
		// reconcile settles; until then the share is stale in exactly the way
		// this file exists to prevent, so it is worth saying.
		c.quiet(ctx, "a cache could not be told about a change", "share", share)
		return
	}

	var (
		dropped []string
		dirs    []string
		changed []Entry
	)
	for p, what := range paths {
		switch {
		case what.gone && what.dir:
			dirs = append(dirs, p)
			continue
		case what.gone:
			dropped = append(dropped, p)
			continue
		}
		// A name the union reserves for itself cannot be held by it, and the
		// fill leaves such a file out for the same reason.
		if overlayMarker(p) {
			c.log().Warn("a file named like an overlay marker is not copied to a share's cache; it is read live",
				"share", share, "path", strings.TrimPrefix(p, "/"))
			continue
		}
		// The size is what bounds a batch. A path that cannot be stat'ed still
		// goes: it may have been deleted again since the event, and a Store
		// leaves out what it cannot open.
		entry := Entry{Path: strings.TrimPrefix(p, "/")}
		if info, err := os.Stat(localPath(root, entry.Path)); err == nil {
			entry.Size = info.Size()
		}
		changed = append(changed, entry)
	}

	ctx, cancel := context.WithTimeout(ctx, invalidateTimeout)
	defer cancel()

	changed = c.withoutLaterConsumerChanges(ctx, share, root, store, changed)
	dropped = append(dropped, c.droppableDirs(ctx, share, root, store, dirs)...)

	if len(dropped) > 0 {
		if err := store.Drop(ctx, share, dropped); err != nil {
			c.quiet(ctx, "dropping from a cache", "share", share, "err", err)
		} else {
			c.unrecord(share, c.shares.recordedUnder(share, dropped))
		}
	}
	for _, batch := range batches(changed) {
		c.record(share, batch)
		skipped, err := store.Apply(ctx, share, root, batch)
		if err != nil {
			c.quiet(ctx, "updating a cache", "share", share, "err", err)
			continue
		}
		if len(skipped) > 0 {
			c.unrecord(share, skipped)
			batch = withoutSkipped(batch, skipped)
		}
		// The manifest means "what this client put in the cache", and a file
		// created here after the fill is exactly that. Left out, it is in the
		// cache and in nobody's record: write-back cannot tell it from the
		// consumer's own file, and a later deletion of it has nothing to
		// reconcile against.
		c.shares.noteSent(share, root, batch)
		c.saveRecord(share)
	}
}

// droppableDirs are the removed directories it is safe to drop from the cache:
// not here again (a drop would take what is being made in it now), and holding
// nothing the container made that write-back has not carried here yet. A
// directory that does hold such a file is kept and said once; write-back
// carries the file, and with it the directory. If the container cannot be
// asked, nothing is dropped: the skeleton is only clutter, and a guess could
// be somebody's work.
func (c *Cache) droppableDirs(ctx context.Context, share, root string, store Store, dirs []string) []string {
	if len(dirs) == 0 {
		return nil
	}
	var gone []string
	for _, d := range dirs {
		if _, err := os.Lstat(localPath(root, d)); errors.Is(err, os.ErrNotExist) {
			gone = append(gone, d)
		}
	}
	if len(gone) == 0 || c.shares.ephemeral(share) {
		return gone
	}
	changed, err := c.consumerChanged(ctx, share, store, true)
	if err != nil {
		return nil
	}
	sent := c.shares.baselines(share)
	var out []string
	for _, d := range gone {
		if p, holds := c.holdsConsumerWork(root, d, changed, sent); holds {
			if c.shares.wb.conflictNew(share, d, "removed here while the container has work in it") {
				c.log().Warn("a directory removed here holds a file the container made that is not carried back yet; the directory is left to write-back",
					"path", strings.TrimPrefix(d, "/"), "file", strings.TrimPrefix(p, "/"))
			}
			continue
		}
		out = append(out, d)
	}
	return out
}

// holdsConsumerWork names a change under dir that is the container's own and
// not yet here.
func (c *Cache) holdsConsumerWork(root, dir string, changed map[string]Change, sent map[string]Baseline) (string, bool) {
	prefix := strings.TrimSuffix(dir, "/") + "/"
	for p, ch := range changed {
		if !strings.HasPrefix(p, prefix) || ch.Deleted || ch.Opaque || ch.Dir {
			continue
		}
		if !c.fillsOwn(root, p, ch, sent) {
			return p, true
		}
	}
	return "", false
}

// withoutLaterConsumerChanges is entries less the paths the consumer changed
// AFTER this machine did. Sending one would overwrite the later version in the
// cache with the earlier, which a bind mount never does; write-back carries it
// here instead, by the same change times (decide). An edit here reaches the
// cache 150ms after it is made, and a container writing in that window is the
// case this exists for.
//
// Only by times both sides know. Without the consumer's change time (an older
// workspace) or this machine's, the edit goes as it always did. The consumer's
// change that loses is said once.
func (c *Cache) withoutLaterConsumerChanges(ctx context.Context, share, root string, store Store, entries []Entry) []Entry {
	if len(entries) == 0 || c.shares.ephemeral(share) || c.ChangeTime == nil {
		return entries
	}
	changed, err := c.consumerChanged(ctx, share, store, true)
	if err != nil || len(changed) == 0 {
		return entries
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return entries
	}
	defer func() { _ = r.Close() }()

	sent := c.shares.sentOf(share, entries)
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		p := slashed(e.Path)
		ch, listed := changed[p]
		if !listed || ch.Changed == 0 || ch.Opaque || ch.Dir || c.fillsOwn(root, p, ch, sent) {
			out = append(out, e)
			continue
		}
		ours, known := c.ChangeTime(r, relPath(p))
		if !known {
			out = append(out, e)
			continue
		}
		if time.Unix(0, ch.Changed).Add(-c.skew()).After(ours.Add(c.margin())) {
			if c.shares.wb.conflictNew(share, p, "the container wrote it after you; write-back carries its version") {
				c.log().Warn("a file changed here and in the container, which wrote it last; its version is carried back rather than replaced by yours",
					"path", strings.TrimPrefix(p, "/"))
			}
			continue
		}
		if c.shares.wb.conflictNew(share, p, "you wrote it after the container") {
			c.log().Warn("a file changed in the container and here, which wrote it last; the container's version is replaced by yours",
				"path", strings.TrimPrefix(p, "/"))
		}
		out = append(out, e)
	}
	return out
}

// withoutSkipped is the entries of a batch the far end did take.
func withoutSkipped(entries []Entry, skipped []string) []Entry {
	left := make(map[string]bool, len(skipped))
	for _, p := range skipped {
		left[slashed(p)] = true
	}
	var out []Entry
	for _, e := range entries {
		if !left[slashed(e.Path)] {
			out = append(out, e)
		}
	}
	return out
}

// excludedPath reports whether any component of a share-relative path is a
// directory the cache does not cover. The fill's own list, so the two cannot
// disagree about what is cacheable.
func excludedPath(p string, excludes []string) bool {
	if len(excludes) == 0 {
		return false
	}
	set := excluded(excludes)
	for _, part := range strings.Split(path.Clean(filepath.ToSlash(p)), "/") {
		if part != "" && set[part] {
			return true
		}
	}
	return false
}
