package fswatch

import (
	"errors"
	"runtime"

	"github.com/fsnotify/fsnotify"
)

// backend is fsnotify's surface, narrowed to what the tree bookkeeping uses,
// so the bookkeeping is tested against a fake with no kernel watches.
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

// Remove tolerates a watch that is already gone: the kernel drops one when its
// directory is deleted, before the event reaches us.
func (b *fsnotifyBackend) Remove(path string) error {
	err := b.w.Remove(path)
	if errors.Is(err, fsnotify.ErrNonExistentWatch) {
		return nil
	}
	return err
}

func (b *fsnotifyBackend) Events() <-chan fsnotify.Event { return b.w.Events }
func (b *fsnotifyBackend) Errors() <-chan error          { return b.w.Errors }
func (b *fsnotifyBackend) Close() error                  { return b.w.Close() }

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
// otherwise. Deliberately short: a large directory is left to the budget,
// which names what it dropped. Build outputs (dist, target, vendor) are NOT
// here, because a container serving dist/ reloading on a change is the point,
// and neither is .gitignore, which commonly ignores exactly that.
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

// ExcludesOr is the exclude list actually applied: DefaultExcludes for nil, an
// explicit list as it stands, an empty one included. The cache and the watcher
// both resolve through it; a cache deciding for itself prefetches a directory
// the watcher never invalidates.
func ExcludesOr(list []string) []string {
	if list == nil {
		return DefaultExcludes
	}
	return list
}
