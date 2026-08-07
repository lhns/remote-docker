package fswatch

import (
	"runtime"

	"github.com/fsnotify/fsnotify"
)

// backend is fsnotify's surface, narrowed to what the tree bookkeeping uses.
//
// The same reason connGate is generic: the policy is the part that gets this
// wrong, and it should be testable without the thing it drives. Watch
// bookkeeping -- when to add, when to prune, what to do when the budget runs
// out -- is exercised against a fake with no kernel watches at all, on a
// machine that has no inotify.
type backend interface {
	Add(path string) error
	Remove(path string) error
	Events() <-chan fsnotify.Event
	Errors() <-chan error
	Close() error
}

type fsnotifyBackend struct{ w *fsnotify.Watcher }

func newFsnotifyBackend() (backend, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	return &fsnotifyBackend{w}, nil
}

func (b *fsnotifyBackend) Add(path string) error { return b.w.Add(path) }

// Remove tolerates a watch that is already gone. The kernel drops a watch when
// its directory is deleted and tells us so with an event, so by the time we
// act on that event the descriptor may already have been reclaimed -- which is
// the ordinary case, not an error.
func (b *fsnotifyBackend) Remove(path string) error {
	err := b.w.Remove(path)
	if err != nil && errorsIsNonExistentWatch(err) {
		return nil
	}
	return err
}

func (b *fsnotifyBackend) Events() <-chan fsnotify.Event { return b.w.Events }
func (b *fsnotifyBackend) Errors() <-chan error          { return b.w.Errors }
func (b *fsnotifyBackend) Close() error                  { return b.w.Close() }

func errorsIsNonExistentWatch(err error) bool {
	return err == fsnotify.ErrNonExistentWatch
}

// DefaultBudget caps how many directories are watched at once.
//
// The limit that actually binds differs per platform and none of them are ours
// to raise:
//
//	linux    fs.inotify.max_user_watches, frequently 8192 in a container
//	windows  one ReadDirectoryChangesW buffer per watch, 64KB each by default
//	darwin   kqueue needs an open fd per watched FILE, not per directory, so
//	         the ceiling is RLIMIT_NOFILE and it arrives very early
//
// These are deliberately below each ceiling. Running out of watches is
// reported here, with the directory named; running out of them in the kernel
// is an opaque ENOSPC in the middle of a walk.
func DefaultBudget() int {
	switch runtime.GOOS {
	case "darwin", "freebsd", "netbsd", "openbsd":
		return 512
	case "windows":
		return 1024
	default:
		return 4096
	}
}

// DefaultExcludes are directory names not watched unless the user says
// otherwise.
//
// Deliberately short. An excluded directory is one nothing reloads on; a
// merely large one is left to the budget, which reports what it dropped and
// can be raised.
//
// Note what is NOT here: dist, build, target, out, vendor. Those are build
// outputs, and a container serving dist/ reloading when dist/ changes is
// exactly the workflow this exists for -- excluding one would reintroduce ADR
// 0014's silent nothing-happens in a narrower and more confusing place. A Rust
// or Maven tree will spend its budget inside target/, and the budget will say
// so by name, which the user can act on.
//
// Honouring .gitignore was considered and rejected: it needs a real ignore
// engine with nested files and negations, it means nothing outside a git
// checkout, and dist/ is both commonly ignored and commonly the thing being
// served.
var DefaultExcludes = []string{
	".git",
	"node_modules",
	".venv",
	"venv",
	"__pycache__",
	".mypy_cache",
	".pytest_cache",
	".gradle",
	".terraform",
}
