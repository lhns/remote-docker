package dircache

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Carrying a consumer's writes back to this machine (ADR 0044). decide.go
// decides; this fetches and writes. Nothing is written back while the cache is
// incomplete, and every conflict decide finds is logged by path.
//
// It never stops for good. A round that cannot finish says why once, waits out
// a backoff and tries again; a file that can never be written here is named once
// and left, not allowed to hold the others back; and what has landed is kept per
// file, so a burst too big for one round finishes over several.

// writeBackEvery is how often a share's changes are collected.
//
// A poll rather than a subscription: the consumer's writes land in a layer the
// store can read at any time, and nothing pushes. Five seconds is short enough
// that a build's output is there before somebody goes looking, and long enough
// not to walk a cache continuously.
const writeBackEvery = 5 * time.Second

// writeBackTimeout bounds one call to the store: one Changes, or one chunk of a
// Pull. Not a whole round, which is bounded by how many files it takes up.
const writeBackTimeout = 2 * time.Minute

const (
	// pullChunk is how many files one Pull asks for, so a slow link costs one
	// chunk's deadline and not the whole burst's.
	pullChunk = 200

	// writeBackRoundFiles caps the files one pass lands. A pass that made
	// progress and left more goes straight on to the next, so the cap bounds
	// how long a round holds the cache and how much is lost to one failure.
	writeBackRoundFiles = 2000

	// maxPassesPerRound bounds the passes one round makes over a share.
	maxPassesPerRound = 50
)

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
		// Bounded, so a share that always has more cannot hold the others: the
		// next tick takes it up again.
		for range maxPassesPerRound {
			if !c.writeBackShare(ctx, share) || ctx.Err() != nil {
				break
			}
		}
	}
}

// budget is how long one call to the store may take.
func (c *Cache) budget() time.Duration {
	if c.roundBudget > 0 {
		return c.roundBudget
	}
	return writeBackTimeout
}

// land writes one file into the share.
//
// guard is asked immediately before the file replaces its target, and a false
// answer leaves the target untouched (errChangedHere).
func (c *Cache) land(r *os.Root, f File, guard func() bool) error {
	if c.writeFile != nil {
		if guard != nil && !guard() {
			return errChangedHere
		}
		return c.writeFile(r, f)
	}
	if err := caseClash(r, relPath(f.Path)); err != nil {
		return err
	}
	return writeUnderGuarded(r, f, guard)
}

// errChangedHere says a path changed on this machine since write-back decided
// about it, so it was not touched.
var errChangedHere = errors.New("changed here since write-back looked at it")

// confirmedAbsent asks the consumer which of the paths a replaced directory
// implies it deleted are really gone from its view, and returns those. Anything
// present, any error, and a store that cannot say all mean keep: the absence of
// a path from the changes proves nothing, because what this machine wrote into
// the directory is hidden from them.
func (c *Cache) confirmedAbsent(ctx context.Context, store Store, share string, candidates []string) []string {
	if len(candidates) == 0 {
		return nil
	}
	pr, ok := store.(Presenter)
	if !ok {
		return nil
	}
	callCtx, cancel := context.WithTimeout(ctx, c.budget())
	defer cancel()
	here, err := pr.Present(callCtx, share, candidates)
	if errors.Is(err, ErrCannotPresent) {
		return nil
	}
	if err != nil {
		c.quiet(ctx, "asking which paths a container still has", "share", share, "err", err)
		return nil
	}
	present := make(map[string]bool, len(here))
	for _, p := range here {
		present[p] = true
	}
	var gone []string
	for _, p := range candidates {
		if !present[p] {
			gone = append(gone, p)
		}
	}
	return gone
}

// writeBackShare collects one share's changes and applies them here. It reports
// whether to go on at once: the pass made progress and left changes behind.
func (c *Cache) writeBackShare(ctx context.Context, share string) bool {
	state, ok := c.shares.get(share)
	root, hasRoot := c.shares.root(share)
	if !ok || !hasRoot {
		return false
	}

	// Still filling means there is nothing settled to compare against. Whether
	// the cache is COMPLETE is the other half, and it goes to decide, which is
	// where the rule and its tests live.
	if !state.Done {
		return false
	}

	store, live := c.Store()
	if !live {
		// Nothing to ask. The changes stay in the cache and the next round
		// collects them.
		return false
	}
	if !c.shares.wbReady(share, c.clock()) {
		return false
	}

	// Before the changes: a reconcile landing after them would take a deleted
	// file out of the record while the changes still list it, and it would
	// come back as the consumer's.
	recorded := c.shares.recordedSet(share)
	callCtx, cancel := context.WithTimeout(ctx, c.budget())
	changes, err := store.Changes(callCtx, share)
	cancel()
	if errors.Is(err, ErrShareGone) {
		// Released, because nothing is bound to it any more (ADR 0044). A far
		// end that merely lost track of a cache it still serves is the Store's
		// to repair before it says this. The cache went with it, so there is
		// nothing to carry back and nothing to compare against: stop polling
		// rather than ask again every five seconds for as long as this cache
		// lives.
		c.shares.forget(share)
		return false
	}
	if err != nil {
		c.stall(ctx, share, root, "asking what a container changed", err, "", -1, false)
		return false
	}

	// What was given up on, and what the union reserves for itself, never
	// reaches decide. reported keeps them: a path set aside is still listed, and
	// is not a deletion a replaced directory implies.
	reported := changes
	changes = c.shares.unskipped(share, changes)
	changes = c.dropMarkers(ctx, share, root, changes)
	if len(changes) == 0 {
		c.shares.wbForgetSaid(share, nil)
		c.recovered(ctx, share, root)
		return false
	}

	// One handle for the round: every path below resolves inside it, links
	// included, so a link in the share cannot lead a write or a delete out.
	r, err := os.OpenRoot(root)
	if err != nil {
		c.stall(ctx, share, root, "opening the share to write back", err, "", -1, false)
		return false
	}
	defer func() { _ = r.Close() }()

	manifest := c.shares.baselines(share)
	changes = c.dropClashes(ctx, share, root, r, manifest, changes)
	for _, p := range c.confirmedAbsent(ctx, store, share, replacedDeletions(manifest, reported)) {
		changes = append(changes, Change{Path: p, Deleted: true})
	}
	c.shares.wbForgetSaid(share, changes)
	actions := decide(manifest, recorded, changes, localAtRoot(r), c.skew(), state.Cached)
	actions = append(actions, decideLinks(manifest, changes, linkAtRoot(r))...)
	if len(actions) == 0 {
		c.recovered(ctx, share, root)
		return false
	}

	for _, conflict := range conflicts(actions) {
		if c.shares.wbConflictNew(share, conflict.Path, conflict.Why) {
			c.log().Warn("a file changed in both places",
				"path", strings.TrimPrefix(conflict.Path, "/"),
				"kind", conflict.kind, "outcome", conflict.Why)
		}
	}

	pass := &writePass{
		c: c, ctx: ctx, share: share, root: root, r: r,
		change: make(map[string]Change, len(changes)),
		action: make(map[string]action, len(actions)),
	}
	for _, ch := range changes {
		pass.change[ch.Path] = ch
	}
	for _, a := range actions {
		pass.action[a.Path] = a
	}
	return pass.run(store, actions)
}

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
	for _, a := range linkActions(actions) {
		p.link(a)
	}

	// Those that have failed least go first, so files that never land cannot
	// take every slot of the pass and starve the ones behind them.
	paths := p.c.shares.wbRanked(p.share, writes(actions))
	more := false
	if len(paths) > writeBackRoundFiles {
		paths, more = paths[:writeBackRoundFiles], true
	}
	p.tried, p.wrote = make(map[string]bool, len(paths)), map[string]bool{}
	for _, path := range paths {
		p.tried[path] = true
	}
	defer func() { p.c.shares.wbTry(p.share, p.tried, p.wrote) }()

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
	if now := linkAtRoot(p.r)(a.Path); now.Exists != a.LinkSeen.Exists || now.IsLink != a.LinkSeen.IsLink ||
		now.Target != a.LinkSeen.Target || !now.ModTime.Equal(a.LinkSeen.ModTime) {
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
	if p.c.shares.wbConflictNew(p.share, path, "changed here while write-back was running") {
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
	var clash *caseClashError
	sk := Skipped{Why: err.Error(), Link: p.change[path].Link != "", Clash: errors.As(err, &clash)}
	p.c.skipped(p.ctx, p.share, p.change[path], sk)
}

// commit records what landed: the baselines both sides now agree on, and the
// record, so a later deletion here can take it out of the cache.
//
// The manifest moves with the files: what was just written back is now what
// both sides agree on, so the next round starts from it rather than seeing the
// same change again.
func (p *writePass) commit(acts []action, added, removed []string) {
	c := p.c
	c.shares.rebase(p.share, p.root, acts)
	c.recording.Lock()
	c.shares.editRecord(p.share, added, true)
	c.shares.editRecord(p.share, removed, false)
	c.recording.Unlock()
	c.shares.wbUnskip(p.share, added, removed)
	c.saveRecord(p.share)
}

// stall says a round could not finish, and when the next try is. Said once, when
// the share stalls; the share is asked again after the backoff whatever else
// happens. A round that made progress waits only the shortest backoff.
func (c *Cache) stall(ctx context.Context, share, root, what string, err error, first string, pending int, progress bool) {
	reason := what + ": " + err.Error()
	newly, next := c.shares.wbFail(share, c.clock(), reason, first, pending, progress)
	if !newly || ctx.Err() != nil || (c.Ctx != nil && c.Ctx.Err() != nil) {
		return
	}
	args := []any{"share", root, "reason", reason, "next try", next.Format(time.TimeOnly)}
	if first != "" {
		args = append(args, "path", strings.TrimPrefix(first, "/"))
	}
	if pending >= 0 {
		args = append(args, "pending", pending)
	}
	c.log().Warn("write-back is stalled; it keeps trying", args...)
}

// recovered says write-back is moving again, if it had stopped.
func (c *Cache) recovered(ctx context.Context, share, root string) {
	if again, since := c.shares.wbOK(share); again && ctx.Err() == nil {
		c.log().Info("write-back is moving again", "share", root,
			"stalled for", c.clock().Sub(since).Round(time.Second))
	}
}

// skipped gives up on a path until it changes, naming it once.
func (c *Cache) skipped(ctx context.Context, share string, ch Change, sk Skipped) {
	if !c.shares.wbSkip(share, ch, sk) || ctx.Err() != nil {
		return
	}
	c.log().Warn("a file cannot be carried back to this machine and is left until it changes",
		"path", strings.TrimPrefix(ch.Path, "/"), "why", sk.Why)
}

// dropMarkers removes what the union reserves for itself from the changes and
// says so once each. A current agent never reports one; an older one reports
// fuse-overlayfs's marker files as the container's, and written into somebody's
// tree they are files they never made.
func (c *Cache) dropMarkers(ctx context.Context, share, root string, changes []Change) []Change {
	var out []Change
	for i, ch := range changes {
		if !overlayMarker(ch.Path) {
			if out != nil {
				out = append(out, ch)
			}
			continue
		}
		if out == nil {
			out = slices.Clone(changes[:i])
		}
		c.skipped(ctx, share, ch, Skipped{Why: "an overlay marker, which is never a file of yours; it is safe to delete"})
	}
	if out == nil {
		return changes
	}
	return out
}

// dropClashes sets aside a file whose name differs only in case from one here,
// on a filesystem that treats the two as one: landing it would replace the other
// with different contents, and decide would otherwise read it as a file that was
// always here and say nothing.
func (c *Cache) dropClashes(ctx context.Context, share, root string, r *os.Root, manifest map[string]Baseline, changes []Change) []Change {
	out := make([]Change, 0, len(changes))
	for _, ch := range changes {
		if _, sent := manifest[ch.Path]; !sent && !ch.Deleted && !ch.Opaque && ch.Link == "" {
			if err := caseClash(r, relPath(ch.Path)); err != nil {
				c.skipped(ctx, share, ch, Skipped{Why: err.Error(), Clash: true})
				continue
			}
		}
		out = append(out, ch)
	}
	return out
}

// caseClash reports a path that exists here under a different spelling.
//
// Only when the name asked for resolves: on a filesystem that tells the two
// apart it does not, and both may exist.
func caseClash(r *os.Root, rel string) error {
	if _, err := r.Lstat(rel); err != nil {
		return nil
	}
	dir, base := filepath.Split(rel)
	if dir == "" {
		dir = "."
	}
	d, err := r.Open(dir)
	if err != nil {
		return nil
	}
	defer func() { _ = d.Close() }()
	names, err := d.Readdirnames(-1)
	if err != nil {
		return nil
	}
	other := ""
	for _, n := range names {
		if n == base {
			return nil
		}
		if strings.EqualFold(n, base) {
			other = n
		}
	}
	if other == "" {
		return nil
	}
	return &caseClashError{path: filepath.ToSlash(rel), existing: other}
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

// linkAtRoot answers what this machine has at a path without following it.
func linkAtRoot(r *os.Root) linkAt {
	return func(p string) linkState {
		rel := relPath(p)
		info, err := r.Lstat(rel)
		if err != nil {
			return linkState{}
		}
		st := linkState{Exists: true, ModTime: info.ModTime()}
		if info.Mode()&os.ModeSymlink != 0 {
			st.IsLink = true
			st.Target, _ = r.Readlink(rel)
		}
		return st
	}
}

// relPath is a share-relative path in this machine's spelling, for os.Root.
func relPath(p string) string {
	return filepath.FromSlash(strings.TrimPrefix(p, "/"))
}

// landLink makes a link, through a temporary name and a rename like a file, so
// one being replaced is never absent. The target is kept as it was written: a
// link is a name, and making one writes nothing through it. What a later write
// does with it is os.Root's to refuse, as it does for every link in the share.
func landLink(r *os.Root, rel, target string) error {
	dir := filepath.Dir(rel)
	if err := r.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(dir, writebackPrefix+strconv.FormatUint(rand.Uint64(), 16))
	if err := r.Symlink(target, tmp); err != nil {
		return err
	}
	if err := r.Rename(tmp, rel); err != nil {
		_ = r.Remove(tmp)
		return err
	}
	return nil
}

// writeUnder writes one file into the share, refusing anything that leaves it.
//
// The store names the path and is not this machine's to trust with one; os.Root
// refuses "..", and a symlink or junction pointing outside the share.
func writeUnder(r *os.Root, file File) error { return writeUnderGuarded(r, file, nil) }

// writeUnderGuarded is writeUnder that asks guard just before the rename, so a
// file that changed here while it was being written is not replaced.
func writeUnderGuarded(r *os.Root, file File, guard func() bool) error {
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
	// change stays pending and the next round tries again. Never a fallback to
	// truncating it.
	// A file already here keeps its own permission bits, as it did when it was
	// written in place: the consumer's copy can differ, and taking its bits
	// would turn a private 0600 file readable or drop an executable bit. Only a
	// new file takes the consumer's.
	perm, existing := file.Mode.Perm(), false
	if info, err := r.Stat(rel); err == nil && info.Mode().IsRegular() {
		perm, existing = info.Mode().Perm(), true
	}

	tmp := filepath.Join(filepath.Dir(rel), writebackPrefix+strconv.FormatUint(rand.Uint64(), 16))
	f, err := r.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	// OpenFile's mode passes through the umask; the bits the file had must not.
	if existing {
		_ = r.Chmod(tmp, perm)
	}

	_, err = io.Copy(f, file.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	// The time the CONSUMER wrote it, which is what a plain mount would have
	// shown, and what the next round compares against.
	if err == nil && !file.ModTime.IsZero() {
		_ = r.Chtimes(tmp, time.Time{}, file.ModTime)
	}
	if err == nil && guard != nil && !guard() {
		err = errChangedHere
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
