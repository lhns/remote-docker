package rewrite

import (
	"errors"
	"strings"
	"testing"

	"github.com/lhns/remote-docker/core/workspace"
)

const watchProject = "/home/alice/project"

// watchRewriter is a rewriter whose watcher starts, counting how often it was
// asked to.
func watchRewriter(def workspace.Watch) (*Rewriter, *fakeSharer, *fakeVolumes, *int) {
	r, sharer, volumes := newRewriter()
	started := new(int)
	r.Watch = def
	r.StartWatcher = func() error { *started++; return nil }
	return r, sharer, volumes, started
}

func bindBody(options string) []byte {
	return []byte(`{"HostConfig":{"Binds":["` + watchProject + `:/app:` + options + `"]}}`)
}

// watch= is a word of the share: it reaches the share, starts the watcher, and
// never reaches the daemon.
func TestWatchWordReachesTheShare(t *testing.T) {
	for _, c := range []struct {
		name    string
		def     workspace.Watch
		options string
		want    workspace.Watch
		starts  int
	}{
		{"named", workspace.WatchOff, "ro,watch=partial", workspace.WatchPartial, 1},
		{"named coarse", workspace.WatchOff, "watch=coarse", workspace.WatchCoarse, 1},
		{"named off against a watching default", workspace.WatchPartial, "watch=off", workspace.WatchOff, 0},
		{"the session default", workspace.WatchCoarse, "ro", workspace.WatchCoarse, 1},
		{"nothing at all", workspace.WatchUnset, "ro", workspace.WatchOff, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, sharer, _, started := watchRewriter(c.def)
			out, err := r.ContainerCreate(t.Context(), bindBody(c.options))
			if err != nil {
				t.Fatalf("ContainerCreate: %v", err)
			}
			if got := sharer.words[watchProject].Watch; got != c.want {
				t.Errorf("shared with watch %q, want %q", got, c.want)
			}
			if *started != c.starts {
				t.Errorf("the watcher was started %d times, want %d", *started, c.starts)
			}
			if strings.Contains(string(out), "watch") {
				t.Errorf("a watch word reached the daemon: %s", out)
			}
		})
	}
}

// A mount that cannot stay coherent without the watcher gets one even when
// the session watches nothing, and the session's own coarse is not lowered.
func TestCachedAndUnionsImplyPartial(t *testing.T) {
	r, sharer, _, started := watchRewriter(workspace.WatchOff)
	if _, err := r.ContainerCreate(t.Context(), bindBody("read=cached")); err != nil {
		t.Fatalf("read=cached with watching off: %v", err)
	}
	if got := sharer.words[watchProject].Watch; got != workspace.WatchPartial || *started != 1 {
		t.Errorf("read=cached resolved to watch %q (started %d times), want partial and one start", got, *started)
	}

	r, sharer, volumes, started := watchRewriter(workspace.WatchOff)
	cache := &fakeCache{}
	r.OpenCache = servingCache(cache)
	r.UnionReady = workspace.UnionReady
	if _, err := r.ContainerCreate(t.Context(), bindBody("write=back")); err != nil {
		t.Fatalf("write=back with watching off: %v", err)
	}
	if got := sharer.words[watchProject].Watch; got != workspace.WatchPartial || *started != 1 || cache.prepared == "" {
		t.Errorf("write=back resolved to watch %q (started %d), prepared %q", got, *started, cache.prepared)
	}
	_ = volumes

	r, sharer, _, _ = watchRewriter(workspace.WatchCoarse)
	if _, err := r.ContainerCreate(t.Context(), bindBody("read=cached")); err != nil {
		t.Fatal(err)
	}
	if got := sharer.words[watchProject].Watch; got != workspace.WatchCoarse {
		t.Errorf("a session default of coarse became %q for read=cached", got)
	}
}

// Asking for off on a mount that needs the watcher is a contradiction, and is
// refused with the way out rather than quietly raised.
func TestWatchOffContradictsWhatNeedsTheWatcher(t *testing.T) {
	for _, c := range []struct{ options, fix string }{
		{"read=cached,watch=off", "fix: drop watch=off, or use read=direct\n"},
		{"write=back,watch=off", "fix: drop watch=off, or use write=through"},
		{"read=cached,write=back,watch=off", "fix: drop watch=off, or use read=direct,write=through"},
	} {
		r, sharer, volumes, started := watchRewriter(workspace.WatchPartial)
		r.OpenCache = servingCache(&fakeCache{})
		r.UnionReady = workspace.UnionReady

		_, err := r.ContainerCreate(t.Context(), bindBody(c.options))
		if err == nil {
			t.Errorf("%s was served", c.options)
			continue
		}
		if !strings.Contains(err.Error(), "watch=off") || !strings.Contains(err.Error(), strings.TrimSuffix(c.fix, "\n")) {
			t.Errorf("%s: err = %q, want the remedy %q", c.options, err, c.fix)
		}
		if len(sharer.shared) != 0 || len(volumes.created) != 0 || *started != 0 {
			t.Errorf("%s: a refused mount shared %v, created %v and started the watcher %d times",
				c.options, sharer.shared, volumes.created, *started)
		}
	}
}

// A watcher that will not start refuses the mount that needed it, naming why,
// and creates nothing.
func TestAWatcherThatWillNotStartRefusesTheMount(t *testing.T) {
	r, sharer, volumes, _ := watchRewriter(workspace.WatchOff)
	r.StartWatcher = func() error { return errors.New("too many open files") }

	_, err := r.ContainerCreate(t.Context(), bindBody("watch=partial"))
	if err == nil || !strings.Contains(err.Error(), "too many open files") || !strings.Contains(err.Error(), watchProject) {
		t.Fatalf("err = %v, want the mount and the cause named", err)
	}
	if len(sharer.shared) != 0 || len(volumes.created) != 0 {
		t.Errorf("a refused mount shared %v and created %v", sharer.shared, volumes.created)
	}

	// A mount that needs no watcher never asks for one.
	if _, err := r.ContainerCreate(t.Context(), bindBody("ro")); err != nil {
		t.Errorf("a plain mount was refused because the watcher cannot start: %v", err)
	}
}

// --mount carries the word in its csv field, and a named volume has it
// stripped and checked like the other words.
func TestWatchWordInMountAndNamedVolume(t *testing.T) {
	r, sharer, _, _ := watchRewriter(workspace.WatchOff)
	if _, err := r.ContainerCreate(t.Context(), []byte(`{"HostConfig":{"Mounts":[`+
		`{"Type":"bind","Source":"`+watchProject+`","Target":"/app","Consistency":"watch=coarse,owner=7"}]}}`)); err != nil {
		t.Fatal(err)
	}
	if w := sharer.words[watchProject]; w.Watch != workspace.WatchCoarse || w.Owner.UID != 7 {
		t.Errorf("shared with %+v", w)
	}

	out, err := r.ContainerCreate(t.Context(), []byte(`{"HostConfig":{"Binds":["data:/d:ro,watch=partial"]}}`))
	if err != nil || strings.Contains(string(out), "watch") {
		t.Errorf("a named volume kept the word: %s, %v", out, err)
	}
	if _, err := r.ContainerCreate(t.Context(), []byte(`{"HostConfig":{"Binds":["data:/d:watch=sometimes"]}}`)); err == nil ||
		!strings.Contains(err.Error(), "off|partial|coarse") {
		t.Errorf("a misspelt watch on a named volume = %v, want the values named", err)
	}
}

// Two mounts of one variant in a request are one share at the strongest watch
// either asked for, and the watcher starts for it even when the first asked for
// none. Nothing is refused.
func TestMountsOfOneVariantRaiseTheWatch(t *testing.T) {
	for _, order := range [][2]string{{"watch=partial", "watch=coarse"}, {"watch=coarse", "watch=partial"}} {
		r, sharer, volumes, _ := watchRewriter(workspace.WatchOff)
		if _, err := r.ContainerCreate(t.Context(), []byte(`{"HostConfig":{"Binds":[`+
			`"`+watchProject+`:/app:`+order[0]+`","`+watchProject+`:/also:`+order[1]+`"]}}`)); err != nil {
			t.Fatalf("%v: %v", order, err)
		}
		if len(volumes.created) != 1 || sharer.exports[0] != sharer.exports[1] {
			t.Errorf("%v: volumes = %v, exports = %v; want one share", order, volumes.created, sharer.exports)
		}
	}

	// The first mount asks for nothing and the second for a watch: the
	// watcher must still start, though the share is already registered.
	r, _, _, started := watchRewriter(workspace.WatchOff)
	if _, err := r.ContainerCreate(t.Context(), []byte(`{"HostConfig":{"Binds":[`+
		`"`+watchProject+`:/app","`+watchProject+`:/also:watch=partial"]}}`)); err != nil {
		t.Fatal(err)
	}
	if *started != 1 {
		t.Errorf("the watcher was started %d times, want once for the second mount", *started)
	}

	// read=cached resolves to partial, so it is a share of its own with the
	// watch that mode needs, beside the one the plain mount named.
	r, sharer, _, _ := watchRewriter(workspace.WatchOff)
	if _, err := r.ContainerCreate(t.Context(), []byte(`{"HostConfig":{"Binds":[`+
		`"`+watchProject+`:/app:watch=partial","`+watchProject+`:/also:read=cached"]}}`)); err != nil {
		t.Fatal(err)
	}
	if sharer.exports[0] == sharer.exports[1] {
		t.Error("read=cached and a plain mount were served as one share")
	}
}
