package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// The config file must never stop existing while Save replaces it.
//
// Save used to unlink the destination before renaming onto it, for a stated
// reason ("Windows will not rename onto an existing file") that is not
// true of os.Rename, which replaces. What it cost was a window in which the
// file was absent, and Load reports a missing file as an empty config with NO
// error, because that is what a machine nobody has configured looks like. So a
// `remote-docker workspace ls` reading in that window printed nothing and
// exited 0, having been told there were no workspaces.
//
// It surfaced as one flaky integration assertion. The same window was open to
// every other reader of that file.
//
// os.Stat rather than Load for the observer: Stat does not hold the file open,
// so this measures the property under test -- does the path ever disappear --
// rather than the contention between a reader and a rename, which on Windows
// is a different and much louder thing.
func TestSaveNeverMakesTheConfigDisappear(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remote-docker.json")

	file := File{Workspaces: map[string]Workspace{
		"dev": {Host: "dev.example", User: "alice"},
	}}
	if err := Save(file, path); err != nil {
		t.Fatalf("seeding the config: %v", err)
	}

	const rounds = 300

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Go(func() {
		defer close(stop)
		for range rounds {
			if err := Save(file, path); err != nil {
				t.Errorf("Save: %v", err)
				return
			}
		}
	})

	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
				t.Error("the config file did not exist during Save: " +
					"it was unlinked before the rename, and Load reads a missing file as an empty config")
				return
			}
		}
	})

	wg.Wait()
}

// And what a reader gets is either the config or an error -- never silently
// nothing. That consequence is what made the absence matter.
func TestLoadDuringSaveIsNeverSilentlyEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remote-docker.json")

	file := File{Workspaces: map[string]Workspace{
		"dev": {Host: "dev.example", User: "alice"},
	}}
	if err := Save(file, path); err != nil {
		t.Fatalf("seeding the config: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// A save every millisecond rather than as fast as the loop will run: the
	// config is written when somebody runs `workspace create`, and a writer
	// with no pause starves the reader on Windows badly enough that the test
	// would measure its own contention instead of the code.
	wg.Go(func() {
		defer close(stop)
		for range 100 {
			if err := Save(file, path); err != nil {
				t.Errorf("Save: %v", err)
				return
			}
			time.Sleep(time.Millisecond)
		}
	})

	var refused int
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			got, err := Load(path)
			if err != nil {
				// Tolerated, and the distinction is the point. On Windows a
				// read landing exactly on the rename is refused with a sharing
				// violation, which is LOUD -- the caller reports it. What must
				// never happen is the quiet answer below.
				refused++
				continue
			}
			if len(got.Workspaces) == 0 {
				t.Error("Load returned an empty config while the file had content")
				return
			}
			time.Sleep(100 * time.Microsecond)
		}
	})

	wg.Wait()

	if refused > 0 {
		t.Logf("%d reads were refused mid-rename (loud, and correct); "+
			"none returned an empty config, which is the claim", refused)
	}
}
