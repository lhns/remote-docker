package dircache

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Carrying a consumer's writes back to this machine (ADR 0044).
//
// What to do is decided in decide.go, a pure function with the rules and their
// tests; what happens here is the fetching and the writing.
//
// Two things it will not do, both because the cost of being wrong is somebody's
// source tree:
//
//   - nothing at all while the cache is incomplete. A file the fill never sent
//     looks exactly like one the consumer created.
//   - nothing silently. Every conflict is reported by path, whichever way it
//     resolved.

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

	actions := decide(c.shares.baselines(share), changes, localAtRoot(root), c.skew(), state.Cached)
	if len(actions) == 0 {
		return
	}

	for _, conflict := range conflicts(actions) {
		c.log().Warn("a file changed in both places",
			"path", strings.TrimPrefix(conflict.Path, "/"),
			"kind", conflict.kind, "outcome", conflict.Why)
	}

	if paths := writes(actions); len(paths) > 0 {
		err := store.Pull(ctx, share, paths, func(f File) error { return writeUnder(root, f) })
		if err != nil {
			c.quiet(ctx, "writing back what a consumer wrote", "share", share, "err", err)
			return
		}
	}

	for _, p := range deletes(actions) {
		target := localPath(root, p)
		if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
			c.quiet(ctx, "removing what a consumer deleted", "path", target, "err", err)
		}
	}

	// The manifest moves with the files: what was just written back is now what
	// both sides agree on, so the next round starts from it rather than seeing
	// the same change again.
	c.shares.rebase(share, root, actions)
}

func (c *Cache) skew() time.Duration {
	if c.Skew == nil {
		return 0
	}
	return c.Skew()
}

// localAtRoot answers what this machine currently has at a share-relative path.
func localAtRoot(root string) localAt {
	return func(p string) (os.FileInfo, bool) {
		info, err := os.Stat(localPath(root, p))
		if err != nil {
			return nil, false
		}
		return info, true
	}
}

// errEscapes is a file naming somewhere outside the share.
var errEscapes = errors.New("dircache: a written-back path leaves the share")

// writeUnder writes one file into the tree, refusing anything that leaves it.
//
// The store named this path, and a store is not this machine's to trust with
// one. Checked on the RESULT, because filepath.Join cleans and "../.." looks
// like an ordinary path afterwards.
func writeUnder(root string, file File) error {
	target := localPath(root, file.Path)
	prefix := strings.TrimSuffix(root, string(filepath.Separator)) + string(filepath.Separator)
	if !strings.HasPrefix(target, prefix) {
		return errEscapes
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, file.Mode.Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, file.Body); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// The time the CONSUMER wrote it, which is what a plain mount would have
	// shown, and what the next round compares against.
	if !file.ModTime.IsZero() {
		_ = os.Chtimes(target, time.Time{}, file.ModTime)
	}
	return nil
}
