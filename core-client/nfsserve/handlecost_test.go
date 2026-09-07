package nfsserve

import (
	"fmt"
	"runtime"
	"testing"

	"github.com/lhns/remote-docker/core/workspace"
)

// What one RPC costs to resolve a handle, as the handle cache fills.
//
// go-nfs resolves every request through CachingHandler.FromHandle, which
// refreshes the LRU recency of the handle's ancestors. That refresh used to
// walk EVERY key in the cache: one allocated slice of all keys, one Peek per
// key, and a Get under the exclusive LRU lock per prefix match. The cache
// holds a million handles (handleCacheSize) and one is minted per path the
// workspace touches, so the cost of resolving a single handle grew with how
// many files the session had ever seen. It is pure CPU, it logs nothing, and
// it gets worse the longer a session runs. Fixed in the fork by reaching the
// ancestors through the reverse path index: see ADR 0047.
//
// The gate counts BYTES ALLOCATED per call rather than time, because the scan
// allocated 16 bytes per cached handle and that number is exact on a loaded CI
// runner where a duration is not. runtime.ReadMemStats stops the world, so
// TotalAlloc is exact. testing.AllocsPerRun is the wrong tool: it counts
// allocation EVENTS, and the slice of every key is one event whatever its size.
func TestPerRPCCostDoesNotGrowWithTheHandleCache(t *testing.T) {
	const (
		small   = 100
		large   = 10_000
		budget  = 4.0
		calls   = 200
		measure = "bytes allocated per FromHandle"
	)

	share := cwdShare(t, t.TempDir())
	srv := New(NewRegistry(DefaultAttrs), nil)

	// A handle deep enough to have ancestors, which is what the refresh walks.
	fh := srv.handler.ToHandle(share.fs, []string{"a", "b", "c", "file"})

	cost := func(cached int) float64 {
		fillTo(t, srv, share, cached)

		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		for range calls {
			if _, _, err := srv.handler.FromHandle(fh); err != nil {
				t.Fatalf("FromHandle failed with %d handles cached: %v", cached, err)
			}
		}
		runtime.ReadMemStats(&after)
		return float64(after.TotalAlloc-before.TotalAlloc) / calls
	}

	atSmall := cost(small)
	atLarge := cost(large)

	ratio := atLarge / atSmall
	t.Logf("%s: %.0f at %d handles, %.0f at %d, ratio %.1f", measure, atSmall, small, atLarge, large, ratio)
	if ratio > budget {
		t.Fatalf("%s grew %.1fx while the cache grew %dx: %.0f at %d handles, %.0f at %d, budget %.1fx",
			measure, ratio, large/small, atSmall, small, atLarge, large, budget)
	}
}

// The ancestor refresh exists so a parent is not evicted while a live child
// still needs it: an evicted ancestor is ESTALE on a path the client is still
// using. The cheap fix would have been to delete the refresh entirely, so pin
// what it is for.
//
// A small cache is the only way to ask this without minting a million handles,
// which is what newServer's limit parameter is for.
func TestFromHandleWorkIsBounded(t *testing.T) {
	const limit = 64

	share := cwdShare(t, t.TempDir())
	srv := newServer(NewRegistry(DefaultAttrs), nil, limit)

	deep := []string{"a", "b", "c", "file"}
	ancestors := map[string][]byte{}
	for i := 1; i < len(deep); i++ {
		ancestors[share.fs.Join(deep[:i]...)] = srv.handler.ToHandle(share.fs, deep[:i])
	}
	fh := srv.handler.ToHandle(share.fs, deep)

	// A client holding the file open while the workspace touches unrelated
	// paths: every mint evicts the least recently used entry once the cache is
	// full, and only the refresh keeps the ancestors out of that position.
	for i := range limit * 4 {
		srv.handler.ToHandle(share.fs, []string{"fill", fmt.Sprint(i)})
		if _, _, err := srv.handler.FromHandle(fh); err != nil {
			t.Fatalf("the open file's own handle went stale after %d unrelated handles: %v", i+1, err)
		}
	}

	for path, ah := range ancestors {
		if _, _, err := srv.handler.FromHandle(ah); err != nil {
			t.Fatalf("ancestor %q was evicted while a handle below it was in use, "+
				"which is ESTALE on a path the client still holds: %v", path, err)
		}
	}
}

// The curve the gate above collapses to one ratio. Not run by `go test`; it is
// what a claim about the cost has to come from.
//
//	go test ./nfsserve -run '^$' -bench BenchmarkFromHandle -benchmem
func BenchmarkFromHandle(b *testing.B) {
	for _, cached := range []int{100, 1_000, 10_000, 50_000} {
		b.Run(fmt.Sprint(cached), func(b *testing.B) {
			share := cwdShare(b, b.TempDir())
			srv := New(NewRegistry(DefaultAttrs), nil)
			fh := srv.handler.ToHandle(share.fs, []string{"a", "b", "c", "file"})
			fillTo(b, srv, share, cached)

			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, _, err := srv.handler.FromHandle(fh); err != nil {
					b.Fatalf("FromHandle failed with %d handles cached: %v", cached, err)
				}
			}
		})
	}
}

// fillTo mints distinct paths until the cache holds at least n handles.
func fillTo(tb testing.TB, srv *Server, share *Share, n int) {
	tb.Helper()
	for i := range n {
		srv.handler.ToHandle(share.fs, []string{"fill", fmt.Sprint(i / 100), fmt.Sprint(i)})
	}
}

// Guard the assumption the tests above rest on: the export they use is the one
// the client addresses, so a handle minted for it is the kind of handle a real
// WRITE presents.
func TestTheMeasuredShareIsTheWorkingDirectoryShare(t *testing.T) {
	share := cwdShare(t, t.TempDir())
	if share.ExportPath != workspace.ExportCWD {
		t.Fatalf("export path %q, want %q", share.ExportPath, workspace.ExportCWD)
	}
}
