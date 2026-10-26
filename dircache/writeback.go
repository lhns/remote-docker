package dircache

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Carrying a consumer's writes back to this machine (ADR 0044). decide.go
// decides; this fetches and writes. Every change the consumer makes comes back
// as a bind mount would have it, last writer wins, and a change that lost is
// named in the log and in status.
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
// something to ask, and the caller knows when that is. It returns as soon as
// ctx ends; the last round before a session closes is Settle's.
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

// WritesBack reports whether any share this cache holds carries the consumer's
// writes back, which is when a closing session has a last round to run.
func (c *Cache) WritesBack() bool {
	for _, share := range c.shares.all() {
		if !c.shares.ephemeral(share) {
			return true
		}
	}
	return false
}

// Settle sends what is waiting to be invalidated, then carries the consumer's
// writes back round after round until one carries nothing, or ctx ends. A
// session runs it before it closes: a change made a moment before the client
// stops (a job ending) is otherwise left in the workspace.
//
// A backoff is not waited out here, since there is no later round. What is left
// when ctx ends, or when a round can carry nothing more, is said per share; it
// stays in the workspace's cache layer, and the next session carries it back.
func (c *Cache) Settle(ctx context.Context) {
	c.flushNow(ctx)
	for {
		carried, last := c.round(ctx, true)
		if carried > 0 && ctx.Err() == nil {
			continue
		}
		for share, r := range last {
			if r.pending == 0 {
				continue
			}
			root, _ := c.shares.root(share)
			args := []any{"share", root, "pending", r.pending}
			if r.first != "" {
				args = append(args, "path", strings.TrimPrefix(r.first, "/"))
			}
			c.log().Warn("write-back did not finish before the session closed; the rest stays in the workspace for the next session", args...)
		}
		return
	}
}

// passResult is what one pass over a share did: whether to go on at once, how
// many changes landed, and how many it left, with the first of them.
type passResult struct {
	more    bool
	landed  int
	pending int
	first   string
}

// writeBackRound asks every share what the consumer changed and carries it
// back.
func (c *Cache) writeBackRound(ctx context.Context) { c.round(ctx, false) }

// round is one write-back round over every share, except an ephemeral one: its
// writes die with it, and it is not even asked, because a build directory would
// otherwise be reported in full to an idle session every few seconds. force
// skips the backoff. It answers how many changes landed, and each share's last
// pass.
func (c *Cache) round(ctx context.Context, force bool) (int, map[string]passResult) {
	c.rounds.Lock()
	defer c.rounds.Unlock()

	carried, last := 0, map[string]passResult{}
	for _, share := range c.shares.all() {
		if c.shares.ephemeral(share) {
			continue
		}
		// Bounded, so a share that always has more cannot hold the others: the
		// next tick takes it up again.
		for range maxPassesPerRound {
			r := c.writeBackShare(ctx, share, force)
			carried += r.landed
			last[share] = r
			if !r.more || ctx.Err() != nil {
				break
			}
		}
	}
	return carried, last
}

// budget is how long one call to the store may take.
func (c *Cache) budget() time.Duration {
	if c.callBudget > 0 {
		return c.callBudget
	}
	return writeBackTimeout
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

// writeBackShare collects one share's changes and applies them here.
func (c *Cache) writeBackShare(ctx context.Context, share string, force bool) passResult {
	root, hasRoot := c.shares.root(share)
	if !hasRoot {
		return passResult{}
	}

	// Not gated on the fill. Whether the fill has finished, or ran out of
	// budget, says nothing about the consumer's changes: a file the cache holds
	// is decided by its baseline, and one it never held by the change times on
	// both sides (decide), and the fill records what it sends before it sends
	// it, so its own copies are never read as the consumer's.
	store, live := c.Store()
	if !live {
		// Nothing to ask. The changes stay in the cache and the next round
		// collects them.
		return passResult{pending: -1}
	}
	if !force && !c.shares.wb.ready(share, c.clock()) {
		return passResult{}
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
		return passResult{}
	}
	if err != nil {
		c.stall(ctx, share, root, "asking what a container changed", err, "", -1, false)
		return passResult{pending: -1}
	}
	c.rememberChanges(share, changes)

	// What was given up on, and what the union reserves for itself, never
	// reaches decide. reported keeps them: a path set aside is still listed, and
	// is not a deletion a replaced directory implies.
	reported := changes
	changes = c.shares.wb.unskipped(share, changes)
	changes = c.dropMarkers(ctx, share, changes)
	if len(changes) == 0 {
		c.shares.wb.forget(share, reported)
		c.recovered(ctx, share, root)
		return passResult{}
	}

	// One handle for the round: every path below resolves inside it, links
	// included, so a link in the share cannot lead a write or a delete out.
	r, err := os.OpenRoot(root)
	if err != nil {
		c.stall(ctx, share, root, "opening the share to write back", err, "", -1, false)
		return passResult{pending: len(changes)}
	}
	defer func() { _ = r.Close() }()

	manifest := c.shares.baselines(share)
	names := c.caseNames(r)
	changes = c.dropClashes(ctx, share, names, manifest, changes)
	changes = append(changes, c.impliedDeletions(ctx, store, share, r, manifest, reported)...)
	c.shares.wb.forget(share, reported, changes)
	actions := decide(manifest, recorded, changes, localAtRoot(r), c.ctimeAtRoot(r), c.skew(), c.shares.exec(share))
	actions = append(actions, decideLinks(manifest, changes, linkAtRoot(r))...)

	pass := &writePass{
		c: c, ctx: ctx, share: share, root: root, r: r, names: names,
		change: make(map[string]Change, len(changes)),
		action: make(map[string]action, len(actions)),
	}
	for _, ch := range changes {
		pass.change[ch.Path] = ch
	}
	for _, a := range actions {
		pass.action[a.Path] = a
	}

	kept := map[string]Skipped{}
	for _, conflict := range ofKind(actions, kindConflict) {
		if !conflict.Wins {
			kept[conflict.Path] = Skipped{Path: conflict.Path, Why: conflict.Why, Kind: SkipKept}
			continue
		}
		if c.shares.wb.conflictNew(share, conflict.Path, conflict.Why) {
			c.log().Warn("a file changed in both places",
				"path", strings.TrimPrefix(conflict.Path, "/"),
				"kind", conflict.kind, "outcome", conflict.Why)
		}
	}
	for _, sk := range c.shares.wb.keptNow(share, kept, pass.change) {
		if ctx.Err() == nil {
			c.log().Warn("a container's change was not carried back; this machine's version is kept",
				"path", strings.TrimPrefix(sk.Path, "/"), "why", sk.Why)
		}
	}

	if len(actions) == len(kept) {
		c.recovered(ctx, share, root)
		return passResult{}
	}
	return pass.run(store, actions)
}

// impliedDeletions are the deletions a directory the consumer removed or
// replaced implies, confirmed against the consumer's own view (OpPresent):
// every file the fill sent under it, and every path this machine has there
// that the cache never held, each carrying the time the directory went so that
// decide keeps what changed here since.
func (c *Cache) impliedDeletions(ctx context.Context, store Store, share string, r *os.Root, manifest map[string]Baseline, reported []Change) []Change {
	hidden := hiddenAt(reported)
	if len(hidden) == 0 {
		return nil
	}
	candidates := replacedDeletions(manifest, reported)
	candidates = append(candidates, c.unheldUnder(share, r, manifest, reported, hidden)...)
	slices.Sort(candidates)
	candidates = slices.Compact(candidates)

	var out []Change
	for _, p := range c.confirmedAbsent(ctx, store, share, candidates) {
		out = append(out, Change{Path: p, Deleted: true, Changed: earliestHidden(p, hidden)})
	}
	return out
}

// hiddenAt is every directory the consumer removed or replaced, by path, with
// the time it did so where the consumer said (0 where not).
func hiddenAt(changes []Change) map[string]int64 {
	hidden := map[string]int64{}
	for _, ch := range changes {
		if ch.Opaque || ch.Deleted {
			hidden[ch.Path] = ch.Changed
		}
	}
	return hidden
}

// earliestHidden is when a path went from the consumer's view: the earliest
// removal or replace of a directory above it. The earliest, because that is
// when it vanished; an edit here after it is this machine's, and kept.
func earliestHidden(p string, hidden map[string]int64) int64 {
	var at int64
	for dir := path.Dir(p); dir != "/" && dir != "."; dir = path.Dir(dir) {
		if t := hidden[dir]; t != 0 && (at == 0 || t < at) {
			at = t
		}
	}
	return at
}

// unheldUnder names what this machine has under a directory the consumer
// removed or replaced that the cache never held: files, links and directories,
// which a bind mount would have removed with it. Only under a directory whose
// removal has a time, since a deletion without one is never acted on. One
// directory is walked once per removal (writeBacks.walked): what this machine
// adds there afterwards is its own and is never a candidate.
func (c *Cache) unheldUnder(share string, r *os.Root, manifest map[string]Baseline, changes []Change, hidden map[string]int64) []string {
	listed := make(map[string]bool, len(changes))
	for _, ch := range changes {
		if !ch.Opaque {
			listed[ch.Path] = true
		}
	}
	var out []string
	for dir, at := range hidden {
		if at == 0 {
			continue
		}
		for _, p := range c.shares.wb.walked(share, dir, at, func() []string { return walkLocal(r, dir) }) {
			if _, held := manifest[p]; held || listed[p] {
				continue
			}
			if _, err := r.Lstat(relPath(p)); err == nil {
				out = append(out, p)
			}
		}
	}
	return out
}

// walkLocal is every path under dir on this machine, never following a link,
// and never write-back's own temporaries.
func walkLocal(r *os.Root, dir string) []string {
	rel := strings.TrimPrefix(dir, "/")
	if info, err := r.Lstat(filepath.FromSlash(rel)); err != nil || !info.IsDir() {
		return nil
	}
	var out []string
	_ = fs.WalkDir(r.FS(), rel, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == rel {
			return nil //nolint:nilerr // an unreadable entry is not a candidate
		}
		if strings.HasPrefix(d.Name(), writebackPrefix) {
			return nil
		}
		out = append(out, "/"+p)
		return nil
	})
	return out
}

// stall says a round could not finish, and when the next try is. Said once, when
// the share stalls; the share is asked again after the backoff whatever else
// happens. A round that made progress waits only the shortest backoff.
func (c *Cache) stall(ctx context.Context, share, root, what string, err error, first string, pending int, progress bool) {
	reason := what + ": " + err.Error()
	newly, next := c.shares.wb.fail(share, c.clock(), reason, first, pending, progress)
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
	if again, since := c.shares.wb.ok(share); again && ctx.Err() == nil {
		c.log().Info("write-back is moving again", "share", root,
			"stalled for", c.clock().Sub(since).Round(time.Second))
	}
}

// skipped gives up on a path until it changes, naming it once.
func (c *Cache) skipped(ctx context.Context, share string, ch Change, sk Skipped) {
	if !c.shares.wb.skip(share, ch, sk) || ctx.Err() != nil {
		return
	}
	c.log().Warn("a file cannot be carried back to this machine and is left until it changes",
		"path", strings.TrimPrefix(ch.Path, "/"), "why", sk.Why)
}

// dropMarkers removes what the union reserves for itself from the changes and
// says so once each. A current agent never reports one; an older one reports
// fuse-overlayfs's marker files as the container's, and written into somebody's
// tree they are files they never made.
func (c *Cache) dropMarkers(ctx context.Context, share string, changes []Change) []Change {
	out := make([]Change, 0, len(changes))
	for _, ch := range changes {
		if overlayMarker(ch.Path) {
			c.skipped(ctx, share, ch, Skipped{Why: "an overlay marker, which is never a file of yours; it is safe to delete", Kind: SkipMarker})
			continue
		}
		out = append(out, ch)
	}
	return out
}

// dropClashes sets aside a file whose name differs only in case from one here,
// on a filesystem that treats the two as one: landing it would replace the other
// with different contents, and decide would otherwise read it as a file that was
// always here.
func (c *Cache) dropClashes(ctx context.Context, share string, names *caseNames, manifest map[string]Baseline, changes []Change) []Change {
	out := make([]Change, 0, len(changes))
	for _, ch := range changes {
		if _, sent := manifest[ch.Path]; !sent && !ch.Deleted && !ch.Opaque && ch.Link == "" {
			if err := names.clash(relPath(ch.Path)); err != nil {
				c.skipped(ctx, share, ch, Skipped{Why: err.Error(), Kind: SkipClash})
				continue
			}
		}
		out = append(out, ch)
	}
	return out
}

// ctimeAtRoot is ChangeTime for share paths under r, or nil when this machine
// cannot say, which decide reads as not known.
func (c *Cache) ctimeAtRoot(r *os.Root) ctimeAt {
	if c.ChangeTime == nil {
		return nil
	}
	return func(p string) (time.Time, bool) { return c.ChangeTime(r, relPath(p)) }
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
//
// guard, if not nil, is asked just before the rename, so a file that changed
// here while it was being written is not replaced (errChangedHere).
func writeUnder(r *os.Root, file File, guard func() bool) error {
	return writeUnderExec(r, file, guard, 0)
}

// writeUnderExec is writeUnder leaving an existing file with the execute bit
// the consumer gave it (action.Exec): +1 set, -1 clear, 0 as it was here.
func writeUnderExec(r *os.Root, file File, guard func() bool, exec int8) error {
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
	// would turn a private 0600 file readable, since the share reports every
	// file wide (ADR 0046). Only a new file takes the consumer's, and an
	// existing one the execute bit the consumer changed.
	perm, existing := file.Mode.Perm(), false
	if info, err := r.Stat(rel); err == nil && info.Mode().IsRegular() {
		perm, existing = withExec(info.Mode().Perm(), exec), true
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
