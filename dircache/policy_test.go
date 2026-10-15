package dircache

import (
	"testing"
	"time"
)

// The policy belongs to the share. Two shares in one cache, one filled from
// the tree and one eagerly, are each filled their own way, and a read feeds the
// tree of the first only.
func TestPoliciesArePerShare(t *testing.T) {
	store := &fakeStore{}
	c, treeRoot := treeCache(t, store)
	_, eagerRoot := treeCache(t, store)

	const tree, eager = "/m/1111111111111111", "/m/2222222222222222"
	c.Attach(tree, treeRoot, ShareOptions{Policy: PolicyTree})
	c.Attach(eager, eagerRoot, ShareOptions{Policy: PolicyEager})

	// Neither is read, so both fill at once, the eager one without being asked
	// and the tree one because nothing is reading. Wait for both before
	// counting, or the tree share's fill lands in the count.
	eventually(t, "both shares to fill", func() bool {
		r := c.Reports()
		return len(r) == 2 && r[0].Done && r[1].Done
	})

	// A read on the eager share reaches no tree: it has already sent.
	before := store.appliedCount()
	c.Touch(eager, "/pkga/faa.go", 2000)
	time.Sleep(50 * time.Millisecond)
	if n := store.appliedCount(); n != before {
		t.Errorf("a read on the eager share sent %d more files", n-before)
	}

	// The tree share is still yielding to reads, so what it holds follows them.
	p := c.prefetchFor(tree)
	if p == nil || p.pol() != PolicyTree {
		t.Fatalf("the tree share's prefetch = %+v", p)
	}
	if q := c.prefetchFor(eager); q == nil || q.pol() != PolicyEager {
		t.Fatalf("the eager share's prefetch = %+v", q)
	}
}

// Touch is on the path of every NFS read, so with no tree share it is one load
// and returns before looking anything up; and forgetting the last tree share
// puts it back there.
func TestTouchKeepsItsFastPath(t *testing.T) {
	store := &fakeStore{}
	c, root := treeCache(t, store)

	c.Touch("/m/1111111111111111", "/pkga/faa.go", 2000)
	if n := c.shares.trees.Load(); n != 0 {
		t.Fatalf("trees = %d on an empty cache", n)
	}

	c.Attach("/m/1111111111111111", root, ShareOptions{Policy: PolicyEager})
	if n := c.shares.trees.Load(); n != 0 {
		t.Errorf("trees = %d with only an eager share, want 0 so Touch returns at once", n)
	}

	_, other := treeCache(t, store)
	c.Attach("/m/2222222222222222", other, ShareOptions{Policy: PolicyTree})
	if n := c.shares.trees.Load(); n != 1 {
		t.Errorf("trees = %d with one tree share, want 1", n)
	}
	c.shares.forget("/m/2222222222222222")
	if n := c.shares.trees.Load(); n != 0 {
		t.Errorf("trees = %d after forgetting it, want 0", n)
	}
}

// A share with policy off finishes at once, whatever its neighbours do.
func TestAShareWithPolicyOffFinishesAtOnce(t *testing.T) {
	store := &fakeStore{}
	c, treeRoot := treeCache(t, store)
	_, offRoot := treeCache(t, store)

	c.Attach("/m/1111111111111111", treeRoot, ShareOptions{Policy: PolicyTree})
	c.Attach("/m/2222222222222222", offRoot, ShareOptions{Policy: PolicyOff})

	for _, r := range c.Reports() {
		if r.Local == offRoot && !r.Done {
			t.Errorf("the share with policy off is still filling: %+v", r)
		}
	}
	if c.prefetchFor("/m/2222222222222222") != nil {
		t.Error("a share with policy off has a prefetch")
	}
}
