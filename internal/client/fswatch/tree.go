package fswatch

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// maxDeniedReported bounds how many distinct subtrees the budget report names.
// Past this the count still rises; only the list stops growing, because a
// report naming ten thousand directories is not a report.
const maxDeniedReported = 16

// shareRoot is one export's local directory, pre-split for matching.
type shareRoot struct {
	export string
	local  string   // the original spelling, for opening
	parts  []string // normalised components, for matching
}

// tree owns which directories are watched.
//
// It holds no lock of its own: the watcher serialises every call, because the
// event loop and Sync both mutate it and the alternative is a second lock
// ordering to get wrong.
type tree struct {
	goos    string
	be      backend
	budget  int
	exclude map[string]bool
	log     Logger

	roots []*shareRoot

	// dirs maps a watched directory to the export it belongs to, keyed by
	// dirKey so a lookup survives the case the OS chooses to report.
	dirs map[string]string

	// denied names the shallowest directory of each subtree the budget
	// refused, with how many refusals happened at or below it.
	denied   map[string]int
	deniedN  int
	excluded int
}

func newTree(goos string, be backend, budget int, exclude []string, log Logger) *tree {
	set := make(map[string]bool, len(exclude))
	for _, name := range exclude {
		set[strings.ToLower(name)] = true
	}
	return &tree{
		goos:    goos,
		be:      be,
		budget:  budget,
		exclude: set,
		log:     log,
		dirs:    make(map[string]string),
		denied:  make(map[string]int),
	}
}

// dirKey normalises a path for use as a map key only. It folds case where the
// local filesystem does, which is exactly what must NOT happen to a path bound
// for the wire.
func dirKey(goos, p string) string {
	joined := "/" + strings.Join(splitLocal(goos, p), "/")
	if caseInsensitive(goos) {
		return strings.ToLower(joined)
	}
	return joined
}

// rootFor finds the share a path belongs to, and its path within that share.
func (t *tree) rootFor(local string) (*shareRoot, string, bool) {
	for _, r := range t.roots {
		if rel, ok := relativeTo(t.goos, r.parts, local); ok {
			return r, rel, true
		}
	}
	return nil, "", false
}

// sync reconciles the watched set against the shares that currently exist.
//
// Written as a diff rather than an append even though nothing unregisters a
// share today, so that it stays correct if something ever does.
func (t *tree) sync(shares []Share) {
	want := make(map[string]bool, len(shares))
	for _, s := range shares {
		want[s.ExportPath] = true
	}

	for _, r := range t.roots {
		if !want[r.export] {
			t.removeTree(r.local)
		}
	}

	have := make(map[string]bool, len(t.roots))
	kept := t.roots[:0]
	for _, r := range t.roots {
		if want[r.export] {
			kept = append(kept, r)
			have[r.export] = true
		}
	}
	t.roots = kept

	for _, s := range shares {
		if have[s.ExportPath] {
			continue
		}
		r := &shareRoot{
			export: s.ExportPath,
			local:  s.LocalPath,
			parts:  splitLocal(t.goos, s.LocalPath),
		}
		t.roots = append(t.roots, r)
		t.addTree(r, r.local, nil)
	}
}

// addTree watches dir and every directory below it.
//
// The walk is breadth-first rather than filepath.WalkDir's depth-first lexical
// order, because it runs against a budget. Depth-first would spend the whole
// budget inside .git/objects/00/ and never reach src/, which is precisely
// backwards: the shallow directories are the ones an edit-reload loop cares
// about.
//
// emit, when non-nil, is called for everything the walk finds. It is used for
// a directory that appeared while we were already running: everything inside
// it was created before we could watch for it, and would otherwise never be
// reported at all. The synthetic events can duplicate real ones that arrive
// the moment after the watch lands, which is harmless -- a replay is a
// notification, not a mutation -- and duplication is much better than the
// silent loss this whole package exists to remove.
func (t *tree) addTree(r *shareRoot, dir string, emit func(path string, isDir bool)) {
	queue := []string{dir}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		if !t.addOne(r, current) {
			continue
		}

		entries, err := os.ReadDir(current)
		if err != nil {
			// A directory that vanished mid-walk is not an error: it was
			// created and removed while we were getting to it, and its own
			// removal event is already on the way.
			if !errors.Is(err, fs.ErrNotExist) {
				t.logf("reading %s: %v", current, err)
			}
			continue
		}

		for _, e := range entries {
			child := filepath.Join(current, e.Name())
			if emit != nil {
				emit(child, e.IsDir())
			}
			// Symlinks are never followed. inotify_add_watch on a symlink
			// watches its target, which would let a watch escape the share
			// that osfs.WithBoundOS() exists to bound -- and admits cycles.
			if e.IsDir() && e.Type()&fs.ModeSymlink == 0 && !t.isExcluded(e.Name()) {
				queue = append(queue, child)
			}
		}
	}
}

// isExcluded also counts, so Stats can report how much of the tree was
// skipped by policy rather than by the budget -- two different reasons a
// change might go unnoticed, and the user needs to tell them apart.
func (t *tree) isExcluded(name string) bool {
	if t.exclude[strings.ToLower(name)] {
		t.excluded++
		return true
	}
	return false
}

// addOne watches a single directory, honouring the budget. It reports whether
// the walk should descend into it.
func (t *tree) addOne(r *shareRoot, dir string) bool {
	key := dirKey(t.goos, dir)
	if _, ok := t.dirs[key]; ok {
		return true
	}
	if len(t.dirs) >= t.budget {
		t.deny(dir)
		return false
	}
	if err := t.be.Add(dir); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			t.logf("watching %s: %v", dir, err)
		}
		return false
	}
	t.dirs[key] = r.export
	return true
}

// deny records a directory the budget refused.
//
// The project rule is that a cap is never silent. Because the walk is
// breadth-first, "the budget ran out" always means the deep parts of the tree
// are uncovered rather than a random half of it, so naming the shallowest
// refused directory of each subtree describes the loss accurately.
func (t *tree) deny(dir string) {
	t.deniedN++
	key := dirKey(t.goos, dir)
	for existing := range t.denied {
		if key == existing || strings.HasPrefix(key, existing+"/") {
			t.denied[existing]++
			return
		}
	}
	if len(t.denied) >= maxDeniedReported {
		return
	}
	t.denied[key] = 1
	t.logf("watch budget of %d directories reached at %s; "+
		"changes below it will not be noticed. Raise it with REMOTE_DOCKER_WATCH_BUDGET "+
		"or exclude directories with REMOTE_DOCKER_WATCH_EXCLUDE.", t.budget, dir)
}

// removeTree drops dir and everything below it.
//
// Needed for a rename and NOT for a delete, and the asymmetry is the subtle
// part. A deleted directory produces IN_DELETE_SELF for each watched
// descendant, so each one arrives as its own event and a prefix scan would
// make `rm -rf node_modules` quadratic. A renamed one produces nothing of the
// sort: kernel watches follow the inode, so events keep arriving spelled with
// the path fsnotify recorded at Add time, and every path we then put on the
// wire is wrong. Dropping the old subtree outright is the only fix; the
// matching event at the new location walks and re-adds it.
func (t *tree) removeTree(dir string) {
	key := dirKey(t.goos, dir)
	for watched := range t.dirs {
		if watched == key || strings.HasPrefix(watched, key+"/") {
			delete(t.dirs, watched)
		}
	}
	_ = t.be.Remove(dir)
}

// removeOne drops a single directory's watch, for a delete.
func (t *tree) removeOne(dir string) {
	delete(t.dirs, dirKey(t.goos, dir))
	_ = t.be.Remove(dir)
}

func (t *tree) watching(dir string) bool {
	_, ok := t.dirs[dirKey(t.goos, dir)]
	return ok
}

func (t *tree) logf(format string, args ...any) {
	if t.log != nil {
		t.log.Printf(format, args...)
	}
}
