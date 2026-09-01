package rewrite

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lhns/remote-docker/core/workspace"
)

// fakeCache stands in for the workspace's union mounts.
type fakeCache struct {
	prepared string // the export it was asked to mount
	cache    string // the volume it was told to use as the cache layer
	port     int
	tree     string // what was applied into it
	err      error
}

func (f *fakeCache) Prepare(_ context.Context, export, cache string, port int) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	f.prepared, f.cache, f.port = export, cache, port
	return "/run/rd-union/testshare/merged", nil
}

func (f *fakeCache) Apply(_ context.Context, _ string, size int64, body io.Reader) error {
	if f.err != nil {
		return f.err
	}
	got, err := io.ReadAll(io.LimitReader(body, size))
	f.tree = string(got)
	return err
}

// cachedRewriter is a rewriter that may serve `cached`, which means one with a
// watcher behind it.
func cachedRewriter() (*Rewriter, *fakeVolumes) {
	r, _, v := newRewriter()
	r.Watching = true
	return r, v
}

// optionsFor is the mount option string the one volume this request created
// was given.
func optionsFor(t *testing.T, v *fakeVolumes) string {
	t.Helper()
	if len(v.created) != 1 {
		t.Fatalf("want exactly one volume, got %d", len(v.created))
	}
	for _, opts := range v.created {
		return opts["o"]
	}
	return ""
}

// The third field of a `-v` is a comma-separated LIST, which is why
// `-v /a:/b:ro,cached` is the spelling. The consistency is consumed and every
// other option is carried through, `ro` above all.
func TestSplitConsistency(t *testing.T) {
	for _, c := range []struct {
		options  string
		want     workspace.Consistency
		leftover string
	}{
		{"", workspace.Unset, ""},
		{"ro", workspace.Unset, "ro"},
		{"cached", workspace.Cached, ""},
		{"ro,cached", workspace.Cached, "ro"},
		{"cached,ro", workspace.Cached, "ro"},
		{"ro,z,delegated", workspace.Delegated, "ro,z"},
		{"consistent", workspace.Consistent, ""},
		// Not a consistency, and not ours to interpret: it goes on to the
		// daemon, which understands more options than this program does.
		{"rw,nocopy", workspace.Unset, "rw,nocopy"},
	} {
		got, leftover := splitConsistency(c.options)
		if got != c.want || leftover != c.leftover {
			t.Errorf("splitConsistency(%q) = %q, %q; want %q, %q",
				c.options, got, leftover, c.want, c.leftover)
		}
	}
}

// The whole of `cached`: the same mount, asked to stop revalidating.
func TestBindConsistencyReachesTheVolume(t *testing.T) {
	r, volumes := cachedRewriter()

	body := []byte(`{"HostConfig":{"Binds":["/home/alice/project:/app:ro,cached"]}}`)
	out, err := r.ContainerCreate(t.Context(), body)
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}

	// The word is ours; ro is the daemon's, and losing it would put a
	// container's writes into the user's project.
	bind := decodeHostConfig(t, out)["Binds"].([]any)[0].(string)
	if !strings.HasSuffix(bind, ":/app:ro") {
		t.Errorf("bind = %q, want the volume mounted read-only with no consistency left", bind)
	}
	if o := optionsFor(t, volumes); !strings.Contains(o, "actimeo=60") {
		t.Errorf("volume options = %q, want the long attribute cache", o)
	}
}

// --mount is the other spelling, and Compose's.
func TestMountConsistencyReachesTheVolume(t *testing.T) {
	r, volumes := cachedRewriter()

	body := []byte(`{"HostConfig":{"Mounts":[` +
		`{"Type":"bind","Source":"/home/alice/project","Target":"/app","Consistency":"cached"}]}}`)
	out, err := r.ContainerCreate(t.Context(), body)
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}

	mount := decodeHostConfig(t, out)["Mounts"].([]any)[0].(map[string]any)
	if mount["Type"] != "volume" {
		t.Errorf("Type = %v, want volume", mount["Type"])
	}
	// Consumed, for the same reason BindOptions is deleted: it describes a
	// bind, and this is a volume now.
	if _, ok := mount["Consistency"]; ok {
		t.Errorf("Consistency survived onto a volume mount: %v", mount)
	}
	if o := optionsFor(t, volumes); !strings.Contains(o, "actimeo=60") {
		t.Errorf("volume options = %q, want the long attribute cache", o)
	}
}

// A mount this program does not rewrite keeps every field it arrived with,
// consistency included: the workspace resolves that path itself (ADR 0041).
func TestAMountTheWorkspaceOwnsKeepsItsConsistency(t *testing.T) {
	r, _ := cachedRewriter()
	r.DaemonPaths = []string{"/lib/modules"}
	r.LocalExists = func(string) bool { return false }

	out, err := r.ContainerCreate(t.Context(), []byte(`{"HostConfig":{"Mounts":[`+
		`{"Type":"bind","Source":"/lib/modules","Target":"/lib/modules","Consistency":"cached"}]}}`))
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	if !strings.Contains(string(out), `"Consistency":"cached"`) {
		t.Errorf("a mount the workspace owns lost its consistency: %s", out)
	}
}

// The workspace setting is what a mount that named nothing gets, and a rule for
// a directory outranks it.
func TestConsistencyPrecedence(t *testing.T) {
	for _, c := range []struct {
		name    string
		def     workspace.Consistency
		paths   map[string]workspace.Consistency
		bind    string
		want    string
		notWant string
	}{
		{
			name: "the workspace default applies when nothing else says",
			def:  workspace.Cached,
			bind: "/home/alice/project:/app",
			want: "actimeo=60",
		},
		{
			name:    "a mount outranks the default",
			def:     workspace.Cached,
			bind:    "/home/alice/project:/app:consistent",
			want:    "actimeo=1,",
			notWant: "nocto",
		},
		{
			name:  "a rule outranks the default",
			def:   workspace.Consistent,
			paths: map[string]workspace.Consistency{"/home/alice": workspace.Cached},
			bind:  "/home/alice/project:/app",
			want:  "actimeo=60",
		},
		{
			// Rules nest, so which one wins cannot depend on map order.
			name: "the deepest rule wins",
			def:  workspace.Consistent,
			paths: map[string]workspace.Consistency{
				"/home/alice":              workspace.Cached,
				"/home/alice/project/live": workspace.Consistent,
			},
			bind:    "/home/alice/project/live:/app",
			want:    "actimeo=1,",
			notWant: "nocto",
		},
		{
			name:  "a rule below the source does not apply to it",
			def:   workspace.Consistent,
			paths: map[string]workspace.Consistency{"/home/alice/project/deep": workspace.Cached},
			bind:  "/home/alice/project:/app",
			want:  "actimeo=1,",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, volumes := cachedRewriter()
			r.Consistency = c.def
			r.ConsistencyPaths = c.paths

			body, err := json.Marshal(map[string]any{
				"HostConfig": map[string]any{"Binds": []string{c.bind}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.ContainerCreate(t.Context(), body); err != nil {
				t.Fatalf("ContainerCreate: %v", err)
			}

			o := optionsFor(t, volumes)
			if !strings.Contains(o, c.want) {
				t.Errorf("volume options = %q, want %q", o, c.want)
			}
			if c.notWant != "" && strings.Contains(o, c.notWant) {
				t.Errorf("volume options = %q, did not want %q", o, c.notWant)
			}
		})
	}
}

// A long attribute cache with nothing to invalidate it is a stale mount, so
// `cached` without a watcher is refused by name rather than quietly served.
func TestCachedNeedsTheWatcher(t *testing.T) {
	r, _, _ := newRewriter()

	_, err := r.ContainerCreate(t.Context(),
		[]byte(`{"HostConfig":{"Binds":["/home/alice/project:/app:cached"]}}`))
	if err == nil {
		t.Fatal("cached was served with no watcher behind it")
	}
	if !strings.Contains(err.Error(), "watch") {
		t.Errorf("error = %v, want the setting named", err)
	}
}

// delegated is not a copy and not a volume the container mounts: the workspace
// mounts a UNION for it, this share's live export underneath and a cache on
// top, and the container binds what that answers with (ADR 0044).
func TestDelegatedMountsAUnion(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "marker"), []byte("cached"), 0o644); err != nil {
		t.Fatal(err)
	}

	r, volumes := cachedRewriter()
	cache := &fakeCache{}
	r.Cache = cache
	r.UnionReady = workspace.UnionReady

	body, err := json.Marshal(map[string]any{
		"Image":      "alpine:3",
		"HostConfig": map[string]any{"Binds": []string{root + ":/app:delegated"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.ContainerCreate(t.Context(), body)
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}

	// What the container binds is the merged path the workspace answered with,
	// not a volume: the union is mounted there, in the daemon's own namespace.
	bind := decodeHostConfig(t, out)["Binds"].([]any)[0].(string)
	if !strings.HasPrefix(bind, "/run/rd-union/") {
		t.Errorf("bind = %q, want the union the workspace mounted", bind)
	}

	// One volume, holding the cache layer, named so the collector can still
	// attribute it to this share (ADR 0029).
	if len(volumes.created) != 1 {
		t.Fatalf("want one volume, got %v", volumes.created)
	}
	for name, opts := range volumes.created {
		if len(opts) != 0 {
			t.Errorf("the cache volume %s was given driver options %v", name, opts)
		}
		if !workspace.IsCacheVolume(name) {
			t.Errorf("%q is not named as a cache layer", name)
		}
		if cache.cache != name {
			t.Errorf("the union was told to use %q, but %q was created", cache.cache, name)
		}
	}

	if cache.port != r.NFSPort {
		t.Errorf("the union was given port %d, want this session's %d", cache.port, r.NFSPort)
	}
	if got := entries(t, strings.NewReader(cache.tree)); got["marker"] != "cached" {
		t.Errorf("the cache was filled with %v, want the file that was there", got)
	}
}

// The mode is refused before anything is created, naming the remedy, rather
// than half way through a container start.
func TestDelegatedRefusesWhatTheWorkspaceCannotServe(t *testing.T) {
	const bind = `{"Image":"alpine","HostConfig":{"Binds":["/home/alice/project:/app:delegated"]}}`

	noChannel, _ := cachedRewriter()
	noChannel.UnionReady = workspace.UnionReady
	if _, err := noChannel.ContainerCreate(t.Context(), []byte(bind)); err == nil ||
		!strings.Contains(err.Error(), string(workspace.Delegated)) {
		t.Fatalf("with no channel: err = %v, want delegated named", err)
	}

	for _, c := range []struct{ reported, want string }{
		{workspace.UnionNoBinary, "WORKSPACE_DIND_IMAGE"},
		{workspace.UnionNoDevice, "/dev/fuse"},
		{"", "update the workspace"},
	} {
		r, _ := cachedRewriter()
		r.Cache = &fakeCache{}
		r.UnionReady = c.reported

		_, err := r.ContainerCreate(t.Context(), []byte(bind))
		if err == nil {
			t.Fatalf("%q was served anyway", c.reported)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q refused with %v, want it to name %q", c.reported, err, c.want)
		}
	}
}

// One directory is one share and one volume, so two mounts of it asking for
// different things can only get one of them. Refused, because the alternative
// is both containers quietly running under whichever was written last.
func TestOneDirectoryCannotHaveTwoConsistencies(t *testing.T) {
	r, _ := cachedRewriter()

	_, err := r.ContainerCreate(t.Context(), []byte(`{"HostConfig":{"Binds":[`+
		`"/home/alice/project:/app:cached","/home/alice/project:/also:consistent"]}}`))
	if err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("err = %v, want a refusal naming the repeat", err)
	}

	// The same directory asking for the same thing twice is ordinary.
	r, volumes := cachedRewriter()
	if _, err := r.ContainerCreate(t.Context(), []byte(`{"HostConfig":{"Binds":[`+
		`"/home/alice/project:/app:cached","/home/alice/project:/also:cached"]}}`)); err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	if o := optionsFor(t, volumes); !strings.Contains(o, "actimeo=60") {
		t.Errorf("volume options = %q", o)
	}
}

// A word nobody else in the toolchain understands is refused where it is
// written, rather than forwarded to a daemon that will report something else.
func TestAMountConsistencyThatIsNotOneIsRefused(t *testing.T) {
	r, _ := cachedRewriter()

	_, err := r.ContainerCreate(t.Context(), []byte(`{"HostConfig":{"Mounts":[`+
		`{"Type":"bind","Source":"/home/alice/project","Target":"/app","Consistency":"fast"}]}}`))
	if err == nil || !strings.Contains(err.Error(), "fast") {
		t.Fatalf("err = %v, want the word named", err)
	}
}

// A single file leaves Binds for Mounts (ADR 0039), and the consistency is
// consumed before that walk: fileMount refuses any option it does not know, so
// a word left in the list would make `-v file:/f:ro,cached` fail on the option
// rather than mount the file.
func TestASingleFileTakesAConsistencyToo(t *testing.T) {
	r, sharer, volumes := newRewriter()
	r.Watching = true
	sharer.files = map[string]string{"/home/alice/app.conf": "app.conf"}

	out, err := r.ContainerCreate(t.Context(),
		[]byte(`{"HostConfig":{"Binds":["/home/alice/app.conf:/etc/app.conf:ro,cached"]}}`))
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}

	mount := decodeHostConfig(t, out)["Mounts"].([]any)[0].(map[string]any)
	if mount["ReadOnly"] != true {
		t.Errorf("ReadOnly = %v, want the flag that keeps a container out of the file", mount["ReadOnly"])
	}
	if o := optionsFor(t, volumes); !strings.Contains(o, "actimeo=60") {
		t.Errorf("volume options = %q, want the long attribute cache", o)
	}
}
