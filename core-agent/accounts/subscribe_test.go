package accounts

import (
	"os"
	"path/filepath"
	"testing"
)

// A subscriber is how a revocation reaches live connections, so it must see the
// map Sync just swapped in, not the one before it.
func TestSubscribeRunsAfterTheSwap(t *testing.T) {
	s := newStore(t)
	key := s.writeKey(t, "alice.pub")
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}

	var calls int
	var authorized bool
	s.Subscribe(func() {
		calls++
		a, _ := s.Lookup("alice")
		authorized = a.Authorized(key)
	})

	if err := os.Remove(filepath.Join(s.keysDir, "alice.pub")); err != nil {
		t.Fatal(err)
	}
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("the subscriber ran %d times for one sync, want 1", calls)
	}
	if authorized {
		t.Error("the subscriber saw alice still authorized: it ran before the swap")
	}

	// A sync that fails swaps nothing, so it tells nobody.
	if err := os.RemoveAll(s.keysDir); err != nil {
		t.Fatal(err)
	}
	if err := s.Sync(); err == nil {
		t.Fatal("Sync succeeded with its operator directory gone")
	}
	if calls != 1 {
		t.Errorf("the subscriber ran after a failed sync: %d calls", calls)
	}
}
