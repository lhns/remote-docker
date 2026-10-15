package dircache

import "testing"

// A later attach can ask for more: off becomes a fill, tree becomes eager, and
// the share's tree-count (Touch's fast path) follows. A weaker one changes
// nothing, and the share is still attached once (ADR 0042).
func TestAttachRaisesThePolicyAndNeverLowersIt(t *testing.T) {
	const share = "/m/1111111111111111"
	store := &fakeStore{}
	c, root := treeCache(t, store)

	c.Attach(share, root, ShareOptions{Policy: PolicyOff})
	eventually(t, "the empty attach to be done", func() bool { r := c.Reports(); return len(r) == 1 && r[0].Done })
	if c.prefetchFor(share) != nil || c.shares.trees.Load() != 0 {
		t.Fatal("an attach with policy off started a prefetch")
	}

	c.Attach(share, root, ShareOptions{Policy: PolicyTree})
	p := c.prefetchFor(share)
	if p == nil || p.pol() != PolicyTree {
		t.Fatalf("off then tree left prefetch %+v, want a tree", p)
	}
	if n := c.shares.trees.Load(); n != 1 {
		t.Errorf("trees = %d after raising to tree, want 1", n)
	}
	eventually(t, "the raised fill to finish", func() bool { r := c.Reports(); return len(r) == 1 && r[0].Done })
	if store.appliedCount() == 0 {
		t.Error("raising off to tree sent nothing")
	}

	c.Attach(share, root, ShareOptions{Policy: PolicyOff})
	c.Attach(share, root, ShareOptions{})
	if q := c.prefetchFor(share); q != p || q.pol() != PolicyTree {
		t.Errorf("a weaker attach changed the prefetch to %+v", q)
	}

	c.Attach(share, root, ShareOptions{Policy: PolicyEager})
	if p.pol() != PolicyEager {
		t.Errorf("tree then eager left %q", p.pol())
	}
	if n := c.shares.trees.Load(); n != 0 {
		t.Errorf("trees = %d once eager, want 0 so Touch returns at once", n)
	}

	c.Attach(share, root, ShareOptions{Policy: PolicyTree})
	if p.pol() != PolicyEager || c.shares.trees.Load() != 0 {
		t.Errorf("eager then tree lowered the policy to %q", p.pol())
	}
	if n := len(c.Reports()); n != 1 {
		t.Errorf("%d reports, want one share", n)
	}
}
