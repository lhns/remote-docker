package dircache

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// What this machine carried back from the consumer is the consumer's own. Once
// the watcher sends a landed file to the cache again (it reads the landing as a
// local change) the agent stops listing it, and an opaque directory above it must
// not read that silence as the consumer deleting it: CI watched 5,000 files made
// in a new directory vanish from the container within seconds, because write-back
// deleted each file it had just landed, and the watcher dropped it from the cache.
func TestACarriedFileIsNotDeletedByTheDirectoryItIsIn(t *testing.T) {
	root := t.TempDir()
	store := &faultStore{}
	store.add("/burst/f1", "made in the container")
	store.changes = append(store.changes, Change{Path: "/burst", Opaque: true})

	c, _, clk := wbCache(t, store, root)
	rounds(t, c, clk, 1)
	if !exists(root, "burst/f1") {
		t.Fatal("the file was not carried back")
	}

	// The watcher sends the landed file to the cache, which is the file's content
	// again: it keeps the consumer's mark.
	c.shares.noteSent(wbShare, root, []Entry{{Path: "burst/f1", Size: int64(len("made in the container"))}})

	// The agent no longer lists it as a change, but the directory is still opaque.
	store.mu.Lock()
	store.changes = []Change{{Path: "/burst", Opaque: true}}
	store.mu.Unlock()
	rounds(t, c, clk, 3)

	if !exists(root, "burst/f1") {
		t.Error("write-back deleted a file it had carried back from the container")
	}
}

// The mark survives a restart with the rest of the baseline.
func TestTheConsumersMarkIsKeptWhenAFileIsSentAgainUnchanged(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var c Cache
	c.shares.set(wbShare, root, &shareState{}, nil)
	b, _ := baselineOf(root, "a")
	b.Consumer = 42
	c.shares.manifests[wbShare]["/a"] = b

	c.shares.noteSent(wbShare, root, []Entry{{Path: "a", Size: 1}})
	if got := c.shares.baselines(wbShare)["/a"].Consumer; got != 42 {
		t.Errorf("consumer mark = %d after an unchanged send, want it kept", got)
	}

	// A file changed here since is no longer the consumer's version.
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(root, "a"), later, later); err != nil {
		t.Fatal(err)
	}
	c.shares.noteSent(wbShare, root, []Entry{{Path: "a", Size: 1}})
	if got := c.shares.baselines(wbShare)["/a"].Consumer; got != 0 {
		t.Errorf("consumer mark = %d after the file changed here, want it dropped", got)
	}
}
