package session

import (
	"testing"

	"github.com/lhns/remote-docker/core/workspace"
)

// The cache may hold only what the watcher covers, so a path under an excluded
// directory has nothing to invalidate: it was never cached, and asking the
// workspace to drop it would be a round trip to say nothing.
func TestExcludedPath(t *testing.T) {
	excludes := []string{".git", "node_modules"}

	for _, c := range []struct {
		path string
		want bool
	}{
		{"/main.go", false},
		{"/pkg/deep/lib.go", false},
		{"/.git/HEAD", true},
		{"/pkg/node_modules/left-pad/index.js", true},

		// A name that merely contains an excluded one is a different
		// directory: .github is not .git, and gitignore is not either.
		{"/.github/workflows/ci.yml", false},
		{"/src/node_modules.md", false},
	} {
		if got := excludedPath(c.path, excludes); got != c.want {
			t.Errorf("excludedPath(%q) = %v, want %v", c.path, got, c.want)
		}
	}

	// No excludes means nothing is skipped, which is what a session with the
	// list emptied deliberately asks for.
	if excludedPath("/.git/HEAD", nil) {
		t.Error("an empty exclude list skipped something")
	}
}

// What the invalidator decides to do with an event, without a workspace to send
// it to: which paths it batches, and whether each is a removal or a rewrite.
func TestInvalidatorBatchesByPath(t *testing.T) {
	const share = "/m/00112233445566ff"
	s := &Session{}
	s.fills.set(share, "/home/alice/project", &fillState{})

	i := &invalidator{session: s}
	defer i.stop()
	observe := func(p string, op workspace.FSOp) {
		i.Observe(workspace.FSEvent{Export: share, Path: p, Op: op})
	}

	observe("/a.go", workspace.OpWrite)
	observe("/b.go", workspace.OpCreate)
	observe("/gone.go", workspace.OpRemove)

	// A rename is a removal of the old name and a creation of the new one, and
	// the last word about a path wins: a file written after being removed is a
	// write, and one removed after being written is a removal.
	observe("/rewritten.go", workspace.OpRemove)
	observe("/rewritten.go", workspace.OpWrite)
	observe("/finally-gone.go", workspace.OpWrite)
	observe("/finally-gone.go", workspace.OpRemove)

	i.mu.Lock()
	pending := i.pending[share]
	i.mu.Unlock()

	for path, wantDeleted := range map[string]bool{
		"/a.go":            false,
		"/b.go":            false,
		"/gone.go":         true,
		"/rewritten.go":    false,
		"/finally-gone.go": true,
	} {
		got, ok := pending[path]
		if !ok {
			t.Errorf("%s was not batched", path)
			continue
		}
		if got != wantDeleted {
			t.Errorf("%s deleted = %v, want %v", path, got, wantDeleted)
		}
	}
}

// A share with no cache costs one map lookup and nothing else. Most shares are
// not delegated, and this runs on the watcher's own path.
func TestInvalidatorIgnoresWhatItDoesNotCache(t *testing.T) {
	s := &Session{}
	s.fills.set("/m/00112233445566ff", "/home/alice/project", &fillState{})
	i := &invalidator{session: s}
	defer i.stop()

	i.Observe(workspace.FSEvent{Export: "/cwd", Path: "/other.go", Op: workspace.OpWrite})

	i.mu.Lock()
	defer i.mu.Unlock()
	if len(i.pending) != 0 {
		t.Errorf("batched %v for a share with no cache", i.pending)
	}
}

// A directory is not cached in its own right: its files are, and each arrives
// as its own event.
func TestInvalidatorIgnoresDirectories(t *testing.T) {
	const share = "/cwd"
	s := &Session{}
	s.fills.set(share, "/home/alice/project", &fillState{})
	i := &invalidator{session: s}
	defer i.stop()

	i.Observe(workspace.FSEvent{Export: share, Path: "/pkg", Op: workspace.OpCreate, Dir: true})

	i.mu.Lock()
	defer i.mu.Unlock()
	if len(i.pending) != 0 {
		t.Errorf("batched %v for a directory", i.pending)
	}
}

// A path the watcher does not cover is never cached, so there is nothing to
// invalidate and no reason to spend a round trip saying so.
func TestInvalidatorIgnoresExcludedPaths(t *testing.T) {
	const share = "/cwd"
	s := &Session{opts: Options{WatchExclude: []string{".git"}}}
	s.fills.set(share, "/home/alice/project", &fillState{})
	i := &invalidator{session: s}
	defer i.stop()

	i.Observe(workspace.FSEvent{Export: share, Path: "/.git/index", Op: workspace.OpWrite})

	i.mu.Lock()
	defer i.mu.Unlock()
	if len(i.pending) != 0 {
		t.Errorf("batched %v for an excluded path", i.pending)
	}
}
