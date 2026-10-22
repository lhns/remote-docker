package dircache

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// A burst larger than one pass finishes over several, and ends: a tar keeps a
// file's time to the second, so what landed must not read as a new change at the
// consumer's nanosecond time and be written again for ever.
func TestABurstBiggerThanOnePassFinishesAndEnds(t *testing.T) {
	root := t.TempDir()
	store := &faultStore{content: map[string]string{}}
	store.mtime = time.Now().Add(time.Minute).Truncate(time.Second)
	n := writeBackRoundFiles + 500
	for i := range n {
		p := fmt.Sprintf("/burst/f%d", i)
		store.content[p] = "x"
		// The consumer's time has nanoseconds; the file that comes back has not.
		store.changes = append(store.changes, Change{Path: p, Size: 1, ModTime: store.mtime.UnixNano() + 123456789})
	}
	c, _, clk := wbCache(t, store, root)

	done := make(chan struct{})
	go func() {
		defer close(done)
		rounds(t, c, clk, 1)
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("write-back is still going after 60s: a landed file reads as a change again and is rewritten for ever")
	}

	entries, _ := os.ReadDir(root + "/burst")
	if len(entries) != n {
		t.Errorf("%d of %d files landed", len(entries), n)
	}
}
