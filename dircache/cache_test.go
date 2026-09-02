package dircache

import "testing"

// A share is released when nothing is bound to it (ADR 0044), and the cache
// goes with it. A session that cannot tell "no union for that" from a transient
// failure asks about it every five seconds for the rest of its life -- which
// the benchmark showed as twelve refusals in the workspace's log for shares
// whose containers had long gone.
func TestSharesForget(t *testing.T) {
	var f shares
	f.set("/m/aaaa", "/home/me/a", &fillState{})
	f.set("/m/bbbb", "/home/me/b", &fillState{})

	f.forget("/m/aaaa")

	if _, ok := f.get("/m/aaaa"); ok {
		t.Error("a forgotten share is still tracked")
	}
	if _, ok := f.roots["/m/aaaa"]; ok {
		t.Error("a forgotten share kept its root")
	}
	if _, ok := f.manifests["/m/aaaa"]; ok {
		t.Error("a forgotten share kept its manifest")
	}
	if _, ok := f.get("/m/bbbb"); !ok {
		t.Error("forgetting one share dropped another")
	}
}
