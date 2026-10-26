package dircache

// One write-back pass over a share (ADR 0044): what writeBackShare decided,
// carried out path by path. The rounds that start passes, and the backoff
// between them, are in writeback.go.

import (
	"cmp"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
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
	names *caseNames

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

// run carries one pass out and says what it did.
func (p *writePass) run(store Store, actions []action) passResult {
	// Deletions, links, directories and modes first: they cost no round trip,
	// and a deletion that waits behind a slow fetch is a file the person sees
	// twice.
	for _, path := range deletes(actions) {
		p.remove(path)
	}
	for _, a := range actions {
		switch a.kind {
		case kindLink:
			p.link(a)
		case kindMkdir:
			p.mkdir(a)
		case kindChmod:
			p.chmod(a)
		}
	}
	p.adopt(ofKind(actions, kindAdopt))

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

	res := passResult{landed: p.landed, pending: max(moving(actions)-p.landed, 0), first: cmp.Or(p.softFirst, p.fetchFirst)}
	// A pass that landed something backs off only the shortest wait whatever
	// else failed: one file that never lands must not slow every other to once
	// a minute.
	switch {
	case p.aborted:
		return res
	case p.fetchErr != nil:
		p.c.stall(p.ctx, p.share, p.root, "fetching what a container wrote", p.fetchErr, p.fetchFirst, p.soft, p.landed > 0)
		return res
	case p.soft > 0:
		p.c.stall(p.ctx, p.share, p.root, "writing back "+strconv.Itoa(p.soft)+" file(s) that could not be written yet",
			p.softErr, p.softFirst, p.soft, p.landed > 0)
		return res
	}
	p.c.recovered(p.ctx, p.share, p.root)
	res.more = more && p.landed > 0
	return res
}

// moving counts the actions that change something here, which is what a pass
// is to land: not a version kept here, and not a baseline adopted.
func moving(actions []action) int {
	n := 0
	for _, a := range actions {
		if a.kind != kindAdopt && (a.kind != kindConflict || a.Wins) {
			n++
		}
	}
	return n
}

// remove deletes a file the consumer deleted, or a directory it removed.
func (p *writePass) remove(path string) {
	a := p.action[path]
	if a.Dir {
		p.removeDir(a)
		return
	}
	if !p.stillAsSeen(a) {
		p.changedHere(path)
		return
	}
	rel := relPath(path)
	err := p.r.Remove(rel)
	if err == nil {
		p.names.removed(rel)
	}
	if err == nil || errors.Is(err, os.ErrNotExist) {
		p.commit([]action{a}, nil, []string{path})
		p.landed++
		return
	}
	p.failed(path, err)
}

// removeDir removes a directory the consumer removed, and only while it is
// empty: the deletions under it run first, deepest first, and anything still in
// it is this machine's and stays, with the directory.
func (p *writePass) removeDir(a action) {
	rel := relPath(a.Path)
	info, err := p.r.Lstat(rel)
	switch {
	case errors.Is(err, os.ErrNotExist):
		p.settled(a.Path)
		return
	case err != nil:
		p.failed(a.Path, err)
		return
	case !info.IsDir():
		p.changedHere(a.Path)
		return
	}
	if err := p.r.Remove(rel); err != nil {
		if !emptyDir(p.r, rel) {
			p.c.sayOnce(p.share, a.Path, "a directory the container removed still holds files here",
				"the container removed a directory that still holds files here; they and the directory are kept")
			return
		}
		p.failed(a.Path, err)
		return
	}
	p.names.removed(rel)
	p.settled(a.Path)
}

// mkdir makes a directory the consumer made.
func (p *writePass) mkdir(a action) {
	if !p.stillAsSeen(a) {
		p.changedHere(a.Path)
		return
	}
	rel := relPath(a.Path)
	err := p.r.MkdirAll(rel, 0o755)
	p.names.wrote(rel, err == nil)
	if err != nil {
		p.failed(a.Path, err)
		return
	}
	p.settled(a.Path)
}

// chmod gives a file here the execute bit the consumer gave it. A filesystem
// that does not keep the bit (a FAT volume) is said once and left, or every
// round would set it again.
func (p *writePass) chmod(a action) {
	if !p.stillAsSeen(a) {
		p.changedHere(a.Path)
		return
	}
	rel := relPath(a.Path)
	info, err := p.r.Stat(rel)
	if err == nil {
		err = p.r.Chmod(rel, withExec(info.Mode().Perm(), a.Exec))
	}
	if err != nil {
		p.failed(a.Path, err)
		return
	}
	if now, err := p.r.Stat(rel); err == nil && (now.Mode()&0o111 != 0) != (a.Exec > 0) {
		p.c.skipped(p.ctx, p.share, p.change[a.Path], Skipped{Why: "this filesystem does not keep an execute bit"})
		return
	}
	p.settled(a.Path)
}

// adopt records the consumer's copies of files this machine has as baselines,
// in one save of the record: a chown -R makes one of every file.
func (p *writePass) adopt(acts []action) {
	var still []action
	var paths []string
	for _, a := range acts {
		if p.stillAsSeen(a) {
			still = append(still, a)
			paths = append(paths, a.Path)
		}
	}
	if len(still) > 0 {
		p.commit(still, paths, nil)
	}
}

// settled records a change that landed and needs no baseline: a directory, or
// a mode.
func (p *writePass) settled(path string) {
	p.c.shares.wb.landed(p.share, []string{path})
	p.landed++
}

// emptyDir reports whether a directory under r has no entries.
func emptyDir(r *os.Root, rel string) bool {
	d, err := r.Open(rel)
	if err != nil {
		return false
	}
	defer func() { _ = d.Close() }()
	names, err := d.Readdirnames(1)
	return len(names) == 0 && errors.Is(err, io.EOF)
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
	rel := relPath(a.Path)
	err := mk(p.r, rel, a.Link)
	p.names.wrote(rel, err == nil)
	if err != nil {
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
		werr := p.land(f, func() bool { return p.stillAsSeen(act) })
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

// land writes one file into the share.
//
// guard is asked immediately before the file replaces its target, and a false
// answer leaves the target untouched (errChangedHere).
func (p *writePass) land(f File, guard func() bool) error {
	if p.c.writeFile != nil {
		if guard != nil && !guard() {
			return errChangedHere
		}
		return p.c.writeFile(p.r, f)
	}
	rel := relPath(f.Path)
	if err := p.names.clash(rel); err != nil {
		return err
	}
	err := writeUnder(p.r, f, guard, p.action[f.Path].Exec)
	p.names.wrote(rel, err == nil)
	return err
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
	now := seenOf(info, err == nil)
	// A decision that read the change time is checked against it too, so a
	// chmod or a rewrite here keeping size and time is still seen.
	if !a.Seen.Ctime.IsZero() && now.Here && p.c.ChangeTime != nil {
		now.Ctime, _ = p.c.ChangeTime(p.r, rel)
	}
	return a.Seen.same(now)
}

// changedHere leaves a path alone because it changed on this machine since write-back
// decided about it. It is looked at again next round, by decide, with the new state; it
// is not a failure, and is said once.
func (p *writePass) changedHere(path string) {
	p.c.sayOnce(p.share, path, "changed here while write-back was running",
		"a file changed here while write-back was running; it was left as it is and is looked at again")
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

// caseNames is what one pass has read of each directory here, to find a name
// that differs only in case from one already here (clash). A directory is
// listed on first use and then kept in step with what the pass itself lands and
// removes, so a pass lists it once rather than once per file. A change made
// here meanwhile is stillAsSeen's to catch, just before each write.
type caseNames struct {
	r    *os.Root
	list func(r *os.Root, dir string) ([]string, error)

	// dirs holds each listed directory's names, by its path ("." for the
	// share's root).
	dirs map[string]map[string]bool
}

func (c *Cache) caseNames(r *os.Root) *caseNames {
	list := readDirNames
	if c.listDir != nil {
		list = c.listDir
	}
	return &caseNames{r: r, list: list, dirs: map[string]map[string]bool{}}
}

// readDirNames lists a directory in the share.
func readDirNames(r *os.Root, dir string) ([]string, error) {
	d, err := r.Open(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = d.Close() }()
	return d.Readdirnames(-1)
}

// clash reports a path that exists here under a different spelling.
//
// Only when the name asked for resolves: on a filesystem that tells the two
// apart it does not, and both may exist.
func (n *caseNames) clash(rel string) error {
	if _, err := n.r.Lstat(rel); err != nil {
		return nil
	}
	dir, base := filepath.Dir(rel), filepath.Base(rel)
	names := n.dirs[dir]
	if names == nil {
		listed, err := n.list(n.r, dir)
		if err != nil {
			return nil
		}
		names = make(map[string]bool, len(listed))
		for _, name := range listed {
			names[name] = true
		}
		n.dirs[dir] = names
	}
	if names[base] {
		return nil
	}
	for name := range names {
		if strings.EqualFold(name, base) {
			return &caseClashError{path: filepath.ToSlash(rel), existing: name}
		}
	}
	return nil
}

// wrote notes a write at rel, which added its name if it landed. Either way
// the directories above it may have been made for it, under the spelling asked
// for or, where case is not told apart, as one already here, so a listing that
// lacks one of them exactly is read again.
func (n *caseNames) wrote(rel string, landed bool) {
	dir := filepath.Dir(rel)
	if names := n.dirs[dir]; names != nil && landed {
		names[filepath.Base(rel)] = true
	}
	for parent := filepath.Dir(dir); parent != dir; dir, parent = parent, filepath.Dir(parent) {
		if names := n.dirs[parent]; names != nil && !names[filepath.Base(dir)] {
			delete(n.dirs, parent)
		}
	}
}

// removed notes a name the pass removed. Removed under another spelling, which
// a filesystem that does not tell case apart allows, the listing is read again.
func (n *caseNames) removed(rel string) {
	dir, base := filepath.Dir(rel), filepath.Base(rel)
	delete(n.dirs, rel)
	switch names := n.dirs[dir]; {
	case names == nil:
	case names[base]:
		delete(names, base)
	default:
		delete(n.dirs, dir)
	}
}
