package dircache

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A cache over a real directory, a fake store, and a context the test owns.
func treeCache(t *testing.T, store *fakeStore) (*Cache, string) {
	t.Helper()
	root := t.TempDir()
	for d := range 4 {
		dir := filepath.Join(root, "pkg"+string(rune('a'+d)))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for f := range 30 {
			name := filepath.Join(dir, "f"+string(rune('a'+f%26))+string(rune('a'+f/26))+".go")
			if err := os.WriteFile(name, make([]byte, 2000+f*50), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	c := &Cache{
		Store:  func() (Store, bool) { return store, true },
		Ctx:    ctx,
		Policy: PolicyTree,
		Link:   func() Link { return Link{RTT: 160 * time.Millisecond, Bandwidth: 12_500_000} },
	}
	return c, root
}

// eventually polls a condition, because the sender and the walk are goroutines.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A read the cache does not hold reaches the tree, and what the tree decides
// reaches the store as one batch, without the read having waited for it.
func TestTouchSendsWhatTheTreeDecides(t *testing.T) {
	store := &fakeStore{}
	c, root := treeCache(t, store)
	c.Attach("/cwd", root, ShareOptions{Prefetch: true})
	// Reads are arriving, so the walk yields and nothing goes before the
	// tree has been asked. A share nobody reads fills at once, by design.
	p := c.prefetchFor("/cwd")
	p.mu.Lock()
	p.lastRead = time.Now()
	p.mu.Unlock()
	eventually(t, "the walk", func() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.walked })

	// Enough reads in one leaf to cross its bar.
	before := store.appliedCount()
	for f := range 10 {
		name := "/pkga/f" + string(rune('a'+f)) + "a.go"
		c.Touch("/cwd", name, 2000)
	}
	eventually(t, "a demand batch", func() bool { return store.appliedCount() > before })

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.applied) == 0 {
		t.Fatal("nothing was applied")
	}
	// The batch is the leaf's remainder: more than the files touched.
	if len(store.applied) <= 10 {
		t.Errorf("the batch held %d files; a leaf promotion brings the files not yet read", len(store.applied))
	}
}

// An ephemeral share is never asked what changed, so nothing of it can ever
// be carried back, and an idle session is not told about a build directory
// every few seconds.
func TestEphemeralSharesAreNeverAskedForChanges(t *testing.T) {
	store := &fakeStore{}
	c, root := treeCache(t, store)
	c.Attach("/cwd", root, ShareOptions{Prefetch: false, Ephemeral: true})
	c.Attach("/m/other", t.TempDir(), ShareOptions{Prefetch: false})

	c.writeBackRound(t.Context())

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.asked["/m/other"] == 0 {
		t.Fatal("the ordinary share was not asked for its changes; the test proves nothing")
	}
	if n := store.asked["/cwd"]; n != 0 {
		t.Errorf("the ephemeral share was asked for its changes %d times", n)
	}
}

// The walk yields: while reads arrive nothing of it is sent, and once they
// stop for the quiet interval the smallest leftovers go, within the budget.
func TestTheWalkYieldsToReads(t *testing.T) {
	store := &fakeStore{}
	c, root := treeCache(t, store)
	c.Attach("/cwd", root, ShareOptions{Prefetch: true})
	p := c.prefetchFor("/cwd")
	// Reads are arriving from the start, or the walk fills the whole share
	// before the test looks, which is what a share nobody reads should do.
	p.mu.Lock()
	p.lastRead = time.Now()
	p.mu.Unlock()
	eventually(t, "the walk", func() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.walked })

	p.mu.Lock()
	p.lastRead = time.Now()
	p.mu.Unlock()
	batch, demand := c.next(p)
	if len(batch) != 0 || demand {
		t.Errorf("with a read just now, next() = %d files, demand=%v; the walk must yield", len(batch), demand)
	}

	p.mu.Lock()
	p.lastRead = time.Now().Add(-2 * quietFor)
	p.mu.Unlock()
	batch, demand = c.next(p)
	if len(batch) == 0 || demand {
		t.Fatalf("after the quiet interval, next() = %d files, demand=%v; the walk should fill the gap", len(batch), demand)
	}
	var size int64
	for i, e := range batch {
		size += e.Size
		if i > 0 && e.Size < batch[i-1].Size {
			t.Errorf("the walk sent %s before a smaller file; smallest first is the rule", e.Path)
		}
	}
	if size > walkBatch {
		t.Errorf("a walk batch of %d bytes exceeds walkBatch %d; a demand batch would wait behind it", size, walkBatch)
	}
}

// Unstored takes the smallest unstored files, marks them, and never the same
// file twice.
func TestUnstoredIsSmallestFirstAndOnce(t *testing.T) {
	entries := smallOnly(sourceTree(10, 25, 5), defaultLargeFile)
	tree, _ := NewTree(entries, Layout{})
	seen := map[string]bool{}
	var last int64 = -1
	for {
		got := tree.Unstored(64<<10, 8)
		if len(got) == 0 {
			break
		}
		for _, e := range got {
			if seen[e.Path] {
				t.Fatalf("%s was handed out twice", e.Path)
			}
			seen[e.Path] = true
			if e.Size < last {
				t.Fatalf("%s (%d) came after a larger file (%d)", e.Path, e.Size, last)
			}
			last = e.Size
		}
	}
	if len(seen) != len(entries) {
		t.Errorf("Unstored handed out %d of %d files", len(seen), len(entries))
	}
	if b, f := tree.Stored(); f != len(entries) {
		t.Errorf("Stored() = %d bytes, %d files after taking everything; want %d files", b, f, len(entries))
	}
}

// Eager ignores reads: a Touch queues nothing, and the walk sends everything
// without waiting for the consumer to go quiet.
func TestPolicyEagerIgnoresReads(t *testing.T) {
	store := &fakeStore{}
	c, root := treeCache(t, store)
	c.Policy = PolicyEager
	c.Attach("/cwd", root, ShareOptions{Prefetch: true})
	p := c.prefetchFor("/cwd")
	p.mu.Lock()
	p.lastRead = time.Now()
	p.mu.Unlock()

	c.Touch("/cwd", "/pkga/faa.go", 2000)
	p.mu.Lock()
	queued := len(p.queue)
	p.mu.Unlock()
	if queued != 0 {
		t.Errorf("a read queued %d demand batches under eager", queued)
	}
	eventually(t, "the whole tree to be sent", func() bool { return store.appliedCount() == 120 })
}
