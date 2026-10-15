package fswatch

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/lhns/remote-docker/core/notify"
	"github.com/lhns/remote-docker/core/workspace"
)

// A write-back lands a file through a temporary of its own; the watcher
// reporting it would invalidate a name that exists for one write.
func TestWriteBackTemporariesAreNotReported(t *testing.T) {
	root := mkdirs(t, t.TempDir(), "src")
	_, be, sink := startWatcher(t, workspace.WatchCoarse, root)

	tmp := filepath.Join(root, "src", writebackPrefix+"1f2e")
	be.events <- fsnotify.Event{Name: tmp, Op: fsnotify.Create}
	be.events <- fsnotify.Event{Name: tmp, Op: fsnotify.Write}
	be.events <- fsnotify.Event{Name: tmp, Op: fsnotify.Rename}
	be.events <- fsnotify.Event{Name: filepath.Join(root, "src", "a.go"), Op: fsnotify.Write}

	waitFor(t, "the real write", func() bool { return len(sink.events()) > 0 })
	time.Sleep(50 * time.Millisecond)

	for _, e := range sink.events() {
		if e.Path != "/src/a.go" || e.Op&notify.OpWrite == 0 {
			t.Errorf("reported %+v, want only the write to /src/a.go", e)
		}
	}
}
