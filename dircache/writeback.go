package dircache

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Carrying a consumer's writes back to this machine (ADR 0044). decide.go
// decides; this fetches and writes. Nothing is written back while the cache is
// incomplete, and every conflict decide finds is logged by path.

// writeBackEvery is how often a share's changes are collected.
//
// A poll rather than a subscription: the consumer's writes land in a layer the
// store can read at any time, and nothing pushes. Five seconds is short enough
// that a build's output is there before somebody goes looking, and long enough
// not to walk a cache continuously.
const writeBackEvery = 5 * time.Second

// writeBackTimeout bounds one round.
const writeBackTimeout = 2 * time.Minute

// WriteBack carries the consumer's writes back until ctx is done.
//
// Per connection rather than per cache: it is worth running only while there is
// something to ask, and the caller knows when that is.
func (c *Cache) WriteBack(ctx context.Context) {
	ticker := time.NewTicker(writeBackEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.writeBackRound(ctx)
		}
	}
}

// writeBackRound asks every share what the consumer changed and carries it
// back, except an ephemeral one: its writes die with it, and it is not even
// asked, because a build directory would otherwise be reported in full to an
// idle session every few seconds.
func (c *Cache) writeBackRound(ctx context.Context) {
	for _, share := range c.shares.all() {
		if c.shares.ephemeral(share) {
			continue
		}
		c.writeBackShare(ctx, share)
	}
}

// writeBackShare collects one share's changes and applies them here.
func (c *Cache) writeBackShare(ctx context.Context, share string) {
	state, ok := c.shares.get(share)
	root, hasRoot := c.shares.root(share)
	if !ok || !hasRoot {
		return
	}

	// Still filling means there is nothing settled to compare against. Whether
	// the cache is COMPLETE is the other half, and it goes to decide, which is
	// where the rule and its tests live.
	if !state.Done {
		return
	}

	store, live := c.Store()
	if !live {
		// Nothing to ask. The changes stay in the cache and the next round
		// collects them.
		return
	}

	ctx, cancel := context.WithTimeout(ctx, writeBackTimeout)
	defer cancel()

	// Before the changes: a reconcile landing after them would take a deleted
	// file out of the record while the changes still list it, and it would
	// come back as the consumer's.
	recorded := c.shares.recordedSet(share)
	changes, err := store.Changes(ctx, share)
	if errors.Is(err, ErrShareGone) {
		// Released, because nothing is bound to it any more (ADR 0044). The
		// cache went with it, so there is nothing to carry back and nothing to
		// compare against: stop polling rather than ask again every five
		// seconds for as long as this cache lives.
		c.shares.forget(share)
		return
	}
	if err != nil {
		c.quiet(ctx, "asking what a consumer changed", "share", share, "err", err)
		return
	}
	if len(changes) == 0 {
		return
	}

	// One handle for the round: every path below resolves inside it, links
	// included, so a link in the share cannot lead a write or a delete out.
	r, err := os.OpenRoot(root)
	if err != nil {
		c.quiet(ctx, "opening a share to write back", "share", share, "err", err)
		return
	}
	defer func() { _ = r.Close() }()

	actions := decide(c.shares.baselines(share), recorded, changes, localAtRoot(r), c.skew(), state.Cached)
	if len(actions) == 0 {
		return
	}

	for _, conflict := range conflicts(actions) {
		c.log().Warn("a file changed in both places",
			"path", strings.TrimPrefix(conflict.Path, "/"),
			"kind", conflict.kind, "outcome", conflict.Why)
	}

	if paths := writes(actions); len(paths) > 0 {
		err := store.Pull(ctx, share, paths, func(f File) error { return writeUnder(r, f) })
		if err != nil {
			c.quiet(ctx, "writing back what a consumer wrote", "share", share, "err", err)
			return
		}
	}

	for _, p := range deletes(actions) {
		if err := r.Remove(relPath(p)); err != nil && !errors.Is(err, os.ErrNotExist) {
			c.quiet(ctx, "removing what a consumer deleted", "path", localPath(root, p), "err", err)
		}
	}

	// The manifest moves with the files: what was just written back is now what
	// both sides agree on, so the next round starts from it rather than seeing
	// the same change again.
	c.shares.rebase(share, root, actions)
	// What came back is in the cache and now here too, so a later deletion
	// here must be able to take it out.
	c.editRecord(share, writes(actions), true)
	c.unrecord(share, deletes(actions))
}

func (c *Cache) skew() time.Duration {
	if c.Skew == nil {
		return 0
	}
	return c.Skew()
}

// localAtRoot answers what this machine currently has at a share-relative path.
// A link leaving the share reads as absent.
func localAtRoot(r *os.Root) localAt {
	return func(p string) (os.FileInfo, bool) {
		info, err := r.Stat(relPath(p))
		if err != nil {
			return nil, false
		}
		return info, true
	}
}

// relPath is a share-relative path in this machine's spelling, for os.Root.
func relPath(p string) string {
	return filepath.FromSlash(strings.TrimPrefix(p, "/"))
}

// writeUnder writes one file into the share, refusing anything that leaves it.
//
// The store names the path and is not this machine's to trust with one; os.Root
// refuses "..", and a symlink or junction pointing outside the share.
func writeUnder(r *os.Root, file File) error {
	rel := relPath(file.Path)
	if err := r.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
		return err
	}

	// Written beside the target and renamed over it, never into it: an editor
	// or a build reading the file meanwhile sees the old one whole, and a
	// stream that fails half way (a dropped link, an agent restart) leaves the
	// user's file as it was instead of truncated.
	//
	// A rename can be refused, on Windows when another program holds the file
	// open without share-delete. That is an error like any other here: the
	// round ends without moving the manifest, so the change is still pending
	// and the next round tries again. Never a fallback to truncating it.
	var f *os.File
	var tmp string
	for range 10 {
		tmp = filepath.Join(filepath.Dir(rel), writebackPrefix+strconv.FormatUint(rand.Uint64(), 16))
		var err error
		f, err = r.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, file.Mode.Perm())
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		f = nil
	}
	if f == nil {
		return errors.New("no free temporary name")
	}

	_, err := io.Copy(f, file.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	// The time the CONSUMER wrote it, which is what a plain mount would have
	// shown, and what the next round compares against.
	if err == nil && !file.ModTime.IsZero() {
		_ = r.Chtimes(tmp, time.Time{}, file.ModTime)
	}
	if err == nil {
		err = r.Rename(tmp, rel)
	}
	if err != nil {
		_ = r.Remove(tmp)
		return err
	}
	return nil
}

// writebackPrefix names the temporary a file is written to before it replaces
// its target. The consumer's tree is the user's, so a name with this prefix is
// ours: the watcher in core-client/fswatch ignores it (it keeps its own copy of
// the string, since this module imports nothing).
const writebackPrefix = ".rd-writeback-"
