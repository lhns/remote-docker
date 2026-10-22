package dircache

// One write-back pass over a share (ADR 0044): what writeBackShare decided,
// carried out path by path. The rounds that start passes, and the backoff
// between them, are in writeback.go.

import (
	"context"
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
)

// writePass is one pass over a share's pending changes.
type writePass struct {
	c     *Cache
	ctx   context.Context
	share string
	root  string
	r     *os.Root

	change map[string]Change
	action map[string]action

	// landed counts what has gone through; soft is what failed in a way that
	// may mend, with the first of it.
	landed    int
	soft      int
	softFirst string
	softErr   error

	// aborted is a pass ended by a full disk, already said.
	aborted bool

	// fetchErr is a pull that failed for a chunk, which the pass goes past.
	fetchErr   error
	fetchFirst string
	chunkFails int

	// tried is every path this pass asked for, and wrote the ones that landed,
	// for ranking next time (wbstate.go).
	tried map[string]bool
	wrote map[string]bool
}

// run carries one pass out and reports whether another should follow at once.
func (p *writePass) run(store Store, actions []action) bool {
	// Deletions and links first: they cost no round trip, and a deletion that
	// waits behind a slow fetch is a file the person sees twice.
	for _, path := range deletes(actions) {
		p.remove(path)
	}
	for _, a := range ofKind(actions, kindLink) {
		p.link(a)
	}

	// Those that have failed least go first, so files that never land cannot
	// take every slot of the pass and starve the ones behind them.
	paths := p.c.shares.wb.ranked(p.share, writes(actions))
	more := false
	if len(paths) > writeBackRoundFiles {
		paths, more = paths[:writeBackRoundFiles], true
	}
	p.tried, p.wrote = make(map[string]bool, len(paths)), map[string]bool{}
	for _, path := range paths {
		p.tried[path] = true
	}
	defer func() { p.c.shares.wb.try(p.share, p.tried, p.wrote) }()

	for chunk := range slices.Chunk(paths, pullChunk) {
		if !p.pull(store, chunk) {
			break
		}
	}

	// A pass that landed something backs off only the shortest wait whatever
	// else failed: one file that never lands must not slow every other to once
	// a minute.
	switch {
	case p.aborted:
		return false
	case p.fetchErr != nil:
		p.c.stall(p.ctx, p.share, p.root, "fetching what a container wrote", p.fetchErr, p.fetchFirst, p.soft, p.landed > 0)
		return false
	case p.soft > 0:
		p.c.stall(p.ctx, p.share, p.root, "writing back "+strconv.Itoa(p.soft)+" file(s) that could not be written yet",
			p.softErr, p.softFirst, p.soft, p.landed > 0)
		return false
	}
	p.c.recovered(p.ctx, p.share, p.root)
	return more && p.landed > 0
}

// remove deletes a file the consumer deleted.
func (p *writePass) remove(path string) {
	if !p.stillAsSeen(p.action[path]) {
		p.changedHere(path)
		return
	}
	err := p.r.Remove(relPath(path))
	if err == nil || errors.Is(err, os.ErrNotExist) {
		p.commit([]action{p.action[path]}, nil, []string{path})
		p.landed++
		return
	}
	p.failed(path, err)
}

// link makes a link the consumer made.
func (p *writePass) link(a action) {
	if !p.stillAsSeen(a) {
		p.changedHere(a.Path)
		return
	}
	mk := landLink
	if p.c.makeLink != nil {
		mk = p.c.makeLink
	}
	if err := mk(p.r, relPath(a.Path), a.Link); err != nil {
		p.failed(a.Path, err)
		return
	}
	p.commit([]action{a}, []string{a.Path}, nil)
	p.landed++
}

// pull fetches one chunk and lands each file as it arrives, so a pull cut short
// keeps what it brought. It reports whether the pass can go on.
func (p *writePass) pull(store Store, chunk []string) bool {
	var (
		wrote []string
		abort error
		first string
	)
	ctx, cancel := context.WithTimeout(p.ctx, p.c.budget())
	err := store.Pull(ctx, p.share, chunk, func(f File) error {
		if f.Unreadable != "" {
			if ch, asked := p.change[f.Path]; asked {
				p.c.skipped(p.ctx, p.share, ch, Skipped{Why: f.Unreadable})
			}
			return nil
		}
		act := p.action[f.Path]
		werr := p.c.land(p.r, f, func() bool { return p.stillAsSeen(act) })
		if werr == nil {
			wrote = append(wrote, f.Path)
			return nil
		}
		if errors.Is(werr, errChangedHere) {
			p.changedHere(f.Path)
			return nil
		}
		if classifyWrite(werr) == faultSpace {
			abort, first = werr, f.Path
			return werr
		}
		p.failed(f.Path, werr)
		return nil
	})
	cancel()

	// Whatever landed moves the manifest and the record now, whether or not the
	// rest of the chunk did.
	acts := make([]action, 0, len(wrote))
	for _, path := range wrote {
		acts = append(acts, p.action[path])
	}
	p.commit(acts, wrote, nil)
	p.landed += len(wrote)
	for _, path := range wrote {
		p.wrote[path] = true
	}

	switch {
	case abort != nil:
		p.aborted = true
		p.c.stall(p.ctx, p.share, p.root, "writing back what a container wrote", abort, first, len(chunk)-len(wrote), false)
		return false
	case err != nil:
		// This chunk is lost for this pass, and the next is tried: one chunk the
		// agent cannot serve must not starve every file behind it. A run of them,
		// or a pass that is being cancelled, is a store that is down.
		if p.fetchErr == nil {
			p.fetchErr, p.fetchFirst = err, chunk[0]
		}
		p.soft += len(chunk) - len(wrote)
		p.chunkFails++
		return p.chunkFails < 3 && p.ctx.Err() == nil
	}
	p.chunkFails = 0
	return true
}

// stillAsSeen reports whether this machine still has at the path what decide
// saw. A pass can run for minutes, so each path is looked at again immediately
// before it is touched.
func (p *writePass) stillAsSeen(a action) bool {
	if a.kind == kindLink {
		return a.LinkSeen.same(linkAtRoot(p.r)(a.Path))
	}
	rel := relPath(a.Path)
	if !a.Seen.Here {
		// Nothing was there; anything now, a dangling link included, is the
		// user's and not ours to replace. A name this machine cannot even look
		// up (Windows answers ERROR_INVALID_NAME) has nothing there either, and
		// the write is what says why it cannot be held.
		_, err := p.r.Lstat(rel)
		return err != nil
	}
	info, err := p.r.Stat(rel)
	return a.Seen.same(seenOf(info, err == nil))
}

// changedHere leaves a path alone because it changed on this machine since write-back
// decided about it. It is looked at again next round, by decide, with the new state; it
// is not a failure, and is said once.
func (p *writePass) changedHere(path string) {
	if p.c.shares.wb.conflictNew(p.share, path, "changed here while write-back was running") {
		p.c.log().Warn("a file changed here while write-back was running; it was left as it is and is looked at again",
			"path", strings.TrimPrefix(path, "/"))
	}
}

// failed sorts a failed write or delete: a path this machine can never hold is
// given up on, anything else is left for the next round.
func (p *writePass) failed(path string, err error) {
	if classifyWrite(err) != faultName {
		p.soft++
		if p.softErr == nil {
			p.softFirst, p.softErr = path, err
		}
		return
	}
	sk := Skipped{Why: err.Error()}
	var clash *caseClashError
	switch {
	case p.change[path].Link != "":
		sk.Kind = SkipLink
	case errors.As(err, &clash):
		sk.Kind = SkipClash
	}
	p.c.skipped(p.ctx, p.share, p.change[path], sk)
}

// commit records what landed: the baselines both sides now agree on, and the
// record, so a later deletion here can take it out of the cache.
//
// The manifest moves with the files: what was just written back is now what
// both sides agree on, so the next round starts from it rather than seeing the
// same change again.
func (p *writePass) commit(acts []action, added, removed []string) {
	f := &p.c.shares
	f.rebase(p.share, p.root, acts)
	f.editRecord(p.share, added, true)
	f.editRecord(p.share, removed, false)
	f.wb.landed(p.share, added, removed)
	p.c.saveRecord(p.share)
}
