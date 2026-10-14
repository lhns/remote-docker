package rewrite

import (
	"strings"
	"testing"

	"github.com/lhns/remote-docker/core/workspace"
)

func prefetchRewriter(def workspace.Prefetch) (*Rewriter, *fakeSharer, *fakeCache) {
	r, sharer, _, _ := watchRewriter(workspace.WatchOff)
	cache := &fakeCache{}
	r.OpenCache = servingCache(cache)
	r.UnionReady = workspace.UnionReady
	r.Prefetch = def
	return r, sharer, cache
}

// prefetch= is a word of the share, reaching the cache's Attach and never the
// daemon; the session default applies only where the mode prefetches.
func TestPrefetchWordReachesTheCache(t *testing.T) {
	for _, c := range []struct {
		name    string
		def     workspace.Prefetch
		options string
		want    workspace.Prefetch
	}{
		{"named", workspace.PrefetchOff, "read=cached,write=back,prefetch=eager", workspace.PrefetchEager},
		{"named off beats the default", workspace.PrefetchTree, "read=cached,write=back,prefetch=off", workspace.PrefetchOff},
		{"the session default", workspace.PrefetchTree, "read=cached,write=back", workspace.PrefetchTree},
		{"nothing at all", workspace.PrefetchUnset, "read=cached,write=back", workspace.PrefetchOff},
		{"a default where the mode does not prefetch", workspace.PrefetchTree, "write=back", workspace.PrefetchOff},
		{"a default on a plain mount", workspace.PrefetchTree, "ro", workspace.PrefetchOff},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, sharer, cache := prefetchRewriter(c.def)
			out, err := r.ContainerCreate(t.Context(), bindBody(c.options))
			if err != nil {
				t.Fatalf("ContainerCreate: %v", err)
			}
			if got := sharer.words[watchProject].Prefetch; got != c.want {
				t.Errorf("shared with prefetch %q, want %q", got, c.want)
			}
			if strings.Contains(c.options, "write=") && cache.words.Prefetch != c.want {
				t.Errorf("the cache was attached with prefetch %q, want %q", cache.words.Prefetch, c.want)
			}
			if strings.Contains(string(out), "prefetch") {
				t.Errorf("a prefetch word reached the daemon: %s", out)
			}
		})
	}
}

// A fill asked for where there is nothing to fill is refused with the way out,
// and off is always accepted.
func TestPrefetchWhereTheModeDoesNotPrefetchIsRefused(t *testing.T) {
	for _, options := range []string{"prefetch=tree", "read=cached,prefetch=eager", "write=back,prefetch=tree"} {
		r, sharer, _ := prefetchRewriter(workspace.PrefetchOff)
		_, err := r.ContainerCreate(t.Context(), bindBody(options))
		if err == nil || !strings.Contains(err.Error(), "fix: drop prefetch=, or use read=cached with write=back or write=ephemeral") {
			t.Errorf("%s: err = %v, want the remedy", options, err)
		}
		if len(sharer.shared) != 0 {
			t.Errorf("%s: a refused mount shared %v", options, sharer.shared)
		}
	}
	r, _, _ := prefetchRewriter(workspace.PrefetchOff)
	if _, err := r.ContainerCreate(t.Context(), bindBody("prefetch=off")); err != nil {
		t.Errorf("prefetch=off on a plain mount was refused: %v", err)
	}
	if _, err := r.ContainerCreate(t.Context(), []byte(`{"HostConfig":{"Binds":["data:/d:prefetch=bogus"]}}`)); err == nil ||
		!strings.Contains(err.Error(), "off|eager|tree") {
		t.Errorf("a misspelt prefetch on a named volume = %v", err)
	}
}
