package session

import (
	"bytes"
	"context"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lhns/remote-docker/client/internal/cachefill"
	"github.com/lhns/remote-docker/core/workspace"
)

// Keeping a delegated share's cache honest (ADR 0044).
//
// A cached copy of a file that has changed here is the one way this mode can be
// WRONG rather than merely slow, and it is worse than a stale attribute: the
// `cached` consistency goes stale for at most actimeo, while an uninvalidated
// cache entry is stale until something removes it. So the watcher is
// load-bearing for correctness here, and this is what it drives.
//
// Both directions go through the merged mount, which is the only defined way to
// change a mounted union -- and has the side effect the whole feature turns on:
// the write is a real operation in the container's own view, so its inotify
// fires natively (ADR 0014).
//
//	changed here -> send the bytes again, overwriting the cached copy
//	deleted here -> remove it, which leaves a whiteout that is correct,
//	                because the lower has lost the file too
//
// The watcher hands these over BEFORE the mode strips anything. A deletion
// cannot be replayed faithfully over NFS, which is what ModePartial is about;
// it can be applied to a cache exactly.

// invalidateDelay batches the events of one editor save, one `git checkout` or
// one build step into a single exchange.
//
// Short enough that an edit reaches a container about as fast as a keystroke
// reaches a file, long enough that writing a thousand files is a handful of
// round trips rather than a thousand.
const invalidateDelay = 150 * time.Millisecond

// invalidator applies the client's changes to the caches of delegated shares.
type invalidator struct {
	session *Session

	mu      sync.Mutex
	pending map[string]map[string]bool // export -> path -> deleted
	timer   *time.Timer
}

// Observe is called for every change the watcher sees, for every share.
//
// Cheap and non-blocking on purpose: it runs on the watcher's own path, and a
// share with no cache -- which is most of them -- costs one map lookup.
func (i *invalidator) Observe(event workspace.FSEvent) {
	local, ok := i.session.cachedShare(event.Export)
	if !ok {
		return
	}
	if event.Dir {
		// A directory is not cached in its own right: its files are, and each
		// of them arrives as its own event. Creating or removing one changes
		// nothing the cache holds.
		return
	}
	// Only what the cache may hold. A path under an excluded directory is
	// served live and has nothing to invalidate, and asking the workspace to
	// drop it would be a round trip to say nothing.
	if excludedPath(event.Path, i.session.opts.WatchExclude) {
		return
	}
	_ = local

	deleted := event.Op&(workspace.OpRemove|workspace.OpRename) != 0

	i.mu.Lock()
	defer i.mu.Unlock()
	if i.pending == nil {
		i.pending = map[string]map[string]bool{}
	}
	if i.pending[event.Export] == nil {
		i.pending[event.Export] = map[string]bool{}
	}
	// A rename looks like a removal of the old name and a creation of the new
	// one, so the last word about a path wins: a file written after being
	// removed is a write, and one removed after being written is a removal.
	i.pending[event.Export][event.Path] = deleted

	if i.timer == nil {
		i.timer = time.AfterFunc(invalidateDelay, i.flush)
	}
}

// flush applies what has accumulated.
func (i *invalidator) flush() {
	i.mu.Lock()
	pending := i.pending
	i.pending, i.timer = nil, nil
	i.mu.Unlock()

	for export, paths := range pending {
		local, ok := i.session.cachedShare(export)
		if !ok {
			continue
		}
		i.apply(export, local, paths)
	}
}

// apply sends one share's changes: the removals as a drop, the rest as a batch
// of their current contents.
func (i *invalidator) apply(export, local string, paths map[string]bool) {
	live := i.session.liveCache()
	if live == nil {
		// Nothing to send it over. The cache is now behind, which the next
		// reconcile settles; until then the share is stale in exactly the way
		// this whole file exists to prevent, so it is worth saying.
		i.session.logQuiet(i.session.ctx, "a cache could not be told about a change", "export", export)
		return
	}

	var (
		dropped []string
		changed []cachefill.Entry
	)
	for p, deleted := range paths {
		if deleted {
			dropped = append(dropped, p)
			continue
		}
		changed = append(changed, cachefill.Entry{Path: strings.TrimPrefix(p, "/")})
	}

	ctx, cancel := context.WithTimeout(i.session.ctx, invalidateTimeout)
	defer cancel()

	if len(dropped) > 0 {
		if err := live.Drop(ctx, export, dropped); err != nil {
			i.session.logQuiet(ctx, "dropping from a cache", "export", export, "err", err)
		}
	}
	if len(changed) > 0 {
		body, err := tarOf(local, changed)
		if err != nil {
			i.session.logQuiet(ctx, "reading a change for a cache", "export", export, "err", err)
			return
		}
		if err := live.Apply(ctx, export, int64(len(body)), bytes.NewReader(body)); err != nil {
			i.session.logQuiet(ctx, "updating a cache", "export", export, "err", err)
		}
	}
}

// invalidateTimeout bounds one exchange. Shorter than a fill's, because this is
// what a person is waiting on: an edit that has not reached the container is
// the thing they are watching for.
const invalidateTimeout = 2 * time.Minute

// cachedShare reports the local directory behind a delegated share, and whether
// there is one.
func (s *Session) cachedShare(export string) (string, bool) {
	s.fills.mu.Lock()
	defer s.fills.mu.Unlock()
	local, ok := s.fills.roots[export]
	return local, ok
}

// excludedPath reports whether any component of a share-relative path is a
// directory the watcher does not cover.
//
// The same list the fill honours, for the same reason: the cache may only hold
// what can be invalidated, so a path nothing watches is served live and is
// never cached in the first place.
func excludedPath(p string, excludes []string) bool {
	if len(excludes) == 0 {
		return false
	}
	set := make(map[string]bool, len(excludes))
	for _, e := range excludes {
		set[strings.TrimSpace(e)] = true
	}
	for _, part := range strings.Split(path.Clean(filepath.ToSlash(p)), "/") {
		if part != "" && set[part] {
			return true
		}
	}
	return false
}
