package dircache

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lhns/remote-docker/core/workspace"
)

// Keeping a cache honest when the tree under it changes (ADR 0044).
//
// A cached copy of a file that has changed here is the one way this mode can be
// WRONG rather than merely slow, and it is worse than a stale attribute: the
// `cached` consistency goes stale for at most actimeo, while an uninvalidated
// cache entry is stale until something removes it. So the change source is
// load-bearing for correctness, and this is what it drives.
//
//	changed here -> send the bytes again, overwriting the cached copy
//	deleted here -> remove it, which leaves a whiteout that is correct,
//	                because the tree underneath has lost the file too
//
// Changes arrive BEFORE the watch mode strips anything. A deletion cannot be
// replayed faithfully over NFS, which is what ModePartial is about; it can be
// applied to a cache exactly.

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
	pending map[string]map[string]bool // share -> path -> deleted
	timer   *time.Timer
}

// Lost is called when the change source could not report everything it saw.
//
// A reconcile rather than a log line, and the difference matters: the events
// that were dropped may have been deletions, and a cached copy of a file that
// is gone shadows its absence for as long as it sits there. Every share this
// cache holds is checked against this machine's own disk, which is local work
// and no round trips unless something really is missing.
func (c *Cache) Lost(notice workspace.FSNotice) {
	c.log().Warn("the watcher dropped changes; checking the caches against this machine",
		"reason", notice.Reason, "dropped", notice.Dropped)

	for _, share := range c.shares.all() {
		root, ok := c.shares.root(share)
		if !ok {
			continue
		}
		// This run's own manifest, not the persisted record: it is what the
		// cache holds right now, where the record is what the last completed
		// fill left. A share still filling has the more accurate of the two.
		go c.dropDeleted(share, root, c.shares.paths(share))
	}
}

// Observe is called for every change seen, for every share.
//
// Cheap and non-blocking on purpose: it runs on the change source's own path,
// and a share with no cache -- which is most of them -- costs one map lookup.
func (c *Cache) Observe(event workspace.FSEvent) {
	if _, ok := c.shares.root(event.Export); !ok {
		return
	}
	if event.Dir {
		// A directory is not cached in its own right: its files are, and each
		// of them arrives as its own event. Creating or removing one changes
		// nothing the cache holds.
		return
	}
	// Only what the cache may hold. A path under an excluded directory is
	// served live and has nothing to invalidate, and asking for it to be
	// dropped would be a round trip to say nothing.
	if excludedPath(event.Path, c.Exclude) {
		return
	}

	deleted := event.Op&(workspace.OpRemove|workspace.OpRename) != 0

	c.inval.mu.Lock()
	defer c.inval.mu.Unlock()
	if c.inval.pending == nil {
		c.inval.pending = map[string]map[string]bool{}
	}
	if c.inval.pending[event.Export] == nil {
		c.inval.pending[event.Export] = map[string]bool{}
	}
	// A rename looks like a removal of the old name and a creation of the new
	// one, so the last word about a path wins: a file written after being
	// removed is a write, and one removed after being written is a removal.
	c.inval.pending[event.Export][event.Path] = deleted

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

// flush applies what has accumulated.
func (c *Cache) flush() {
	c.inval.mu.Lock()
	pending := c.inval.pending
	c.inval.pending, c.inval.timer = nil, nil
	c.inval.mu.Unlock()

	for share, paths := range pending {
		root, ok := c.shares.root(share)
		if !ok {
			continue
		}
		c.invalidate(share, root, paths)
	}
}

// invalidate sends one share's changes: the removals as a drop, the rest as a
// batch of their current contents.
func (c *Cache) invalidate(share, root string, paths map[string]bool) {
	store, ok := c.Store()
	if !ok {
		// Nowhere to send it. The cache is now behind, which the next
		// reconcile settles; until then the share is stale in exactly the way
		// this file exists to prevent, so it is worth saying.
		c.quiet(c.Ctx, "a cache could not be told about a change", "share", share)
		return
	}

	var (
		dropped []string
		changed []Entry
	)
	for p, deleted := range paths {
		if deleted {
			dropped = append(dropped, p)
			continue
		}
		// The size is what bounds a batch. A path that cannot be stat'ed still
		// goes: it may have been deleted again since the event, and a Store
		// leaves out what it cannot open.
		entry := Entry{Path: strings.TrimPrefix(p, "/")}
		if info, err := os.Stat(filepath.Join(root, filepath.FromSlash(entry.Path))); err == nil {
			entry.Size = info.Size()
		}
		changed = append(changed, entry)
	}

	ctx, cancel := context.WithTimeout(c.Ctx, invalidateTimeout)
	defer cancel()

	if len(dropped) > 0 {
		if err := store.Drop(ctx, share, dropped); err != nil {
			c.quiet(ctx, "dropping from a cache", "share", share, "err", err)
		}
	}
	for _, batch := range batches(changed) {
		if err := store.Apply(ctx, share, root, batch); err != nil {
			c.quiet(ctx, "updating a cache", "share", share, "err", err)
			continue
		}
		// The manifest means "what this client put in the cache", and a file
		// created here after the fill is exactly that. Left out, it is in the
		// cache and in nobody's record: write-back cannot tell it from the
		// consumer's own file, and a later deletion of it has nothing to
		// reconcile against.
		c.shares.noteSent(share, root, batch)
	}
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
