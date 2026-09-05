package rewrite

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lhns/remote-docker/core/workspace"
)

// fakeCache stands in for the workspace's union mounts.
type fakeCache struct {
	prepared string         // the export it was asked to mount
	cache    string         // the volume it was told to use as the cache layer
	port     int            //
	read     workspace.Read // the read mode the lower was to be mounted with
	attached string         // the export it was told about
	from     string         // the directory behind it
	mode     workspace.Mode // the mode it was told the share has
	err      error
}

func (f *fakeCache) Prepare(_ context.Context, export, cache string, port int, mode workspace.Mode) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	f.prepared, f.cache, f.port, f.read = export, cache, port, mode.Read
	return "/run/rd-union/testshare/merged", nil
}

// Attach is asynchronous in the real one; here it records what it was told,
// which is what the tests are about.
func (f *fakeCache) Attach(export, localPath string, mode workspace.Mode) {
	f.attached, f.from, f.mode = export, localPath, mode
}

// cachedRewriter is a rewriter that may serve `read=cached`, which means one
// with a watcher behind it.
func cachedRewriter() (*Rewriter, *fakeVolumes) {
	r, _, v := newRewriter()
	r.Watching = true
	return r, v
}

// unionRewriter may serve a union as well: a cache channel and a workspace
// that reported it can.
func unionRewriter() (*Rewriter, *fakeVolumes, *fakeCache) {
	r, v := cachedRewriter()
	c := &fakeCache{}
	r.Cache = c
	r.UnionReady = workspace.UnionReady
	return r, v, c
}

var (
	cachedThrough = workspace.Mode{Read: workspace.ReadCached, Write: workspace.WriteThrough}
	cachedBack    = workspace.Mode{Read: workspace.ReadCached, Write: workspace.WriteBack}
	cachedOnly    = workspace.Mode{Read: workspace.ReadCached}
)

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
// `-v /a:/b:ro,cached` is the spelling. Every mode word is consumed, in every
// spelling, and every other option is carried through, `ro` above all.
func TestSplitMode(t *testing.T) {
	for _, c := range []struct {
		options  string
		want     workspace.Mode
		leftover string
	}{
		{"", workspace.ModeUnset, ""},
		{"ro", workspace.ModeUnset, "ro"},
		{"read=cached", cachedOnly, ""},
		{"ro,read=cached", cachedOnly, "ro"},
		{"read=cached,ro", cachedOnly, "ro"},
		{"ro,z,read=cached,write=back", cachedBack, "ro,z"},
		{"read=direct,write=through", workspace.DefaultMode, ""},
		{"read=cached,write=back", cachedBack, ""},
		{"ro,read=direct,write=ephemeral", workspace.Mode{Read: workspace.ReadDirect, Write: workspace.WriteEphemeral}, "ro"},
		{"write=ephemeral,ro,read=cached", workspace.Mode{Read: workspace.ReadCached, Write: workspace.WriteEphemeral}, "ro"},
		{"read=cached,write=ephemeral,ro", workspace.Mode{Read: workspace.ReadCached, Write: workspace.WriteEphemeral}, "ro"},
		// One axis alone leaves the other for the rule or the default.
		{"ro,write=back", workspace.Mode{Write: workspace.WriteBack}, "ro"},
		// Docker's own words name both axes.
		{"ro,cached", cachedThrough, "ro"},
		{"delegated", cachedBack, ""},
		// Not a mode, and not ours to interpret: it goes on to the daemon,
		// which understands more options than this program does.
		{"rw,nocopy", workspace.ModeUnset, "rw,nocopy"},
	} {
		got, leftover, err := splitMode(c.options)
		if err != nil {
			t.Errorf("splitMode(%q): %v", c.options, err)
			continue
		}
		if got != c.want || leftover != c.leftover {
			t.Errorf("splitMode(%q) = %v, %q; want %v, %q",
				c.options, got, leftover, c.want, c.leftover)
		}
	}

	// A word that looks like ours and is not is refused naming it.
	if _, _, err := splitMode("ro,read=fast"); err == nil || !strings.Contains(err.Error(), "fast") {
		t.Errorf("splitMode(read=fast) = %v, want the word named", err)
	}
	// A repeated axis in a list is refused, as it is in a configured value.
	if _, _, err := splitMode("read=cached,write=through,read=direct"); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Errorf("splitMode(read twice) = %v, want a refusal naming the repeat", err)
	}
}

// The whole of `cached`: the same mount, asked to stop revalidating.
func TestBindModeReachesTheVolume(t *testing.T) {
	for _, spelling := range []string{"read=cached", "read=cached,write=through", "write=through,read=cached"} {
		r, volumes := cachedRewriter()

		body := []byte(`{"HostConfig":{"Binds":["/home/alice/project:/app:ro,` + spelling + `"]}}`)
		out, err := r.ContainerCreate(t.Context(), body)
		if err != nil {
			t.Fatalf("%s: ContainerCreate: %v", spelling, err)
		}

		// The words are ours; ro is the daemon's, and losing it would put a
		// container's writes into the user's project.
		bind := decodeHostConfig(t, out)["Binds"].([]any)[0].(string)
		if !strings.HasSuffix(bind, ":/app:ro") {
			t.Errorf("%s: bind = %q, want the volume mounted read-only with no mode word left", spelling, bind)
		}
		if o := optionsFor(t, volumes); !strings.Contains(o, "actimeo=60") {
			t.Errorf("%s: volume options = %q, want the long attribute cache", spelling, o)
		}
	}
}

// --mount is the other spelling, and Compose's. Docker's field, our value,
// and the value has to be one word because the CLI splits --mount on commas
// before this is seen.
func TestMountModeReachesTheVolume(t *testing.T) {
	for _, value := range []string{"read=cached", "read=cached,write=through"} {
		r, volumes := cachedRewriter()

		body := []byte(`{"HostConfig":{"Mounts":[` +
			`{"Type":"bind","Source":"/home/alice/project","Target":"/app","Consistency":"` + value + `"}]}}`)
		out, err := r.ContainerCreate(t.Context(), body)
		if err != nil {
			t.Fatalf("%s: ContainerCreate: %v", value, err)
		}

		mount := decodeHostConfig(t, out)["Mounts"].([]any)[0].(map[string]any)
		if mount["Type"] != "volume" {
			t.Errorf("%s: Type = %v, want volume", value, mount["Type"])
		}
		// Consumed, for the same reason BindOptions is deleted: it describes
		// a bind, and this is a volume now.
		if _, ok := mount["Consistency"]; ok {
			t.Errorf("%s: Consistency survived onto a volume mount: %v", value, mount)
		}
		if o := optionsFor(t, volumes); !strings.Contains(o, "actimeo=60") {
			t.Errorf("%s: volume options = %q, want the long attribute cache", value, o)
		}
	}
}

// A mount this program does not rewrite keeps every field it arrived with,
// consistency included: the workspace resolves that path itself (ADR 0041),
// and Docker's own word is inert there.
func TestAMountTheWorkspaceOwnsIsForwardedWhole(t *testing.T) {
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

// The workspace setting is what a mount that named nothing gets, a rule for a
// directory outranks it, and each axis is filled on its own.
func TestModePrecedence(t *testing.T) {
	for _, c := range []struct {
		name    string
		def     workspace.Mode
		paths   map[string]workspace.Mode
		bind    string
		want    string
		notWant string
	}{
		{
			name: "the workspace default applies when nothing else says",
			def:  cachedThrough,
			bind: "/home/alice/project:/app",
			want: "actimeo=60",
		},
		{
			name:    "a mount outranks the default",
			def:     cachedThrough,
			bind:    "/home/alice/project:/app:read=direct",
			want:    "actimeo=1,",
			notWant: "nocto",
		},
		{
			name:  "a rule outranks the default",
			def:   workspace.DefaultMode,
			paths: map[string]workspace.Mode{"/home/alice": cachedThrough},
			bind:  "/home/alice/project:/app",
			want:  "actimeo=60",
		},
		{
			// Rules nest, so which one wins cannot depend on map order.
			name: "the deepest rule wins",
			def:  workspace.DefaultMode,
			paths: map[string]workspace.Mode{
				"/home/alice":              cachedThrough,
				"/home/alice/project/live": workspace.DefaultMode,
			},
			bind:    "/home/alice/project/live:/app",
			want:    "actimeo=1,",
			notWant: "nocto",
		},
		{
			name:  "a rule below the source does not apply to it",
			def:   workspace.DefaultMode,
			paths: map[string]workspace.Mode{"/home/alice/project/deep": cachedThrough},
			bind:  "/home/alice/project:/app",
			want:  "actimeo=1,",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, volumes := cachedRewriter()
			r.Mode = c.def
			r.ModePaths = c.paths

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
// `read=cached` without a watcher is refused by name rather than quietly
// served.
func TestCachedNeedsTheWatcher(t *testing.T) {
	r, _, _ := newRewriter()

	_, err := r.ContainerCreate(t.Context(),
		[]byte(`{"HostConfig":{"Binds":["/home/alice/project:/app:read=cached"]}}`))
	if err == nil {
		t.Fatal("cached was served with no watcher behind it")
	}
	if !strings.Contains(err.Error(), "watch") {
		t.Errorf("error = %v, want the setting named", err)
	}
}

// A union is not a copy and not a volume the container mounts: the workspace
// mounts one for it, this share's live export underneath and a cache on top,
// and the container binds what that answers with (ADR 0044). It exists
// exactly when writes are not synchronous, and its lower gets the share's
// read mode.
func TestAUnionIsChosenByTheWriteAxis(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "marker"), []byte("cached"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		spelling string
		mode     workspace.Mode
		read     workspace.Read
	}{
		{"read=cached,write=back", cachedBack, workspace.ReadCached},
		{"write=back,read=cached", cachedBack, workspace.ReadCached},
		{"read=direct,write=back", workspace.Mode{Read: workspace.ReadDirect, Write: workspace.WriteBack}, workspace.ReadDirect},
		{"read=direct,write=ephemeral", workspace.Mode{Read: workspace.ReadDirect, Write: workspace.WriteEphemeral}, workspace.ReadDirect},
		{"read=cached,write=ephemeral", workspace.Mode{Read: workspace.ReadCached, Write: workspace.WriteEphemeral}, workspace.ReadCached},
	} {
		t.Run(c.spelling, func(t *testing.T) {
			r, volumes, cache := unionRewriter()

			body, err := json.Marshal(map[string]any{
				"Image":      "alpine:3",
				"HostConfig": map[string]any{"Binds": []string{root + ":/app:" + c.spelling}},
			})
			if err != nil {
				t.Fatal(err)
			}
			out, err := r.ContainerCreate(t.Context(), body)
			if err != nil {
				t.Fatalf("ContainerCreate: %v", err)
			}

			// What the container binds is the merged path the workspace
			// answered with, not a volume: the union is mounted there, in the
			// daemon's own namespace.
			bind := decodeHostConfig(t, out)["Binds"].([]any)[0].(string)
			if !strings.HasPrefix(bind, "/run/rd-union/") {
				t.Errorf("bind = %q, want the union the workspace mounted", bind)
			}

			// One volume, holding the cache layer, named so the collector can
			// still attribute it to this share (ADR 0029).
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
			// The lower's attribute cache follows the READ axis.
			if cache.read != c.read {
				t.Errorf("the lower was to be mounted %s, want %s", cache.read, c.read)
			}
			// The share is attached with its whole mode, and not waited on:
			// the container is already running against a live lower.
			if cache.attached != cache.prepared || cache.from != root {
				t.Errorf("attached %q from %q, want %q from %q",
					cache.attached, cache.from, cache.prepared, root)
			}
			if cache.mode != c.mode {
				t.Errorf("attached with %v, want %v", cache.mode, c.mode)
			}
		})
	}
}

// The Mounts form of the same thing binds the merged path as a BIND, not a
// volume: handing Docker a volume name of /run/rd-union/... created a volume
// by that name and mounted an empty directory.
func TestAUnionInTheMountsFormIsABind(t *testing.T) {
	r, _, _ := unionRewriter()

	body := []byte(`{"HostConfig":{"Mounts":[` +
		`{"Type":"bind","Source":"/home/alice/project","Target":"/app","Consistency":"read=cached,write=back"}]}}`)
	out, err := r.ContainerCreate(t.Context(), body)
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	mount := decodeHostConfig(t, out)["Mounts"].([]any)[0].(map[string]any)
	if mount["Type"] != "bind" {
		t.Errorf("Type = %v, want bind: the merged path is a directory in the daemon's namespace", mount["Type"])
	}
	if src, _ := mount["Source"].(string); !strings.HasPrefix(src, "/run/rd-union/") {
		t.Errorf("Source = %v, want the union the workspace mounted", mount["Source"])
	}
	if _, ok := mount["Consistency"]; ok {
		t.Errorf("Consistency survived: %v", mount)
	}
}

// The mode is refused before anything is created, naming the remedy, rather
// than half way through a container start.
func TestAUnionIsRefusedWhereTheWorkspaceCannotServeIt(t *testing.T) {
	const bind = `{"Image":"alpine","HostConfig":{"Binds":["/home/alice/project:/app:read=cached,write=back"]}}`

	noChannel, _ := cachedRewriter()
	noChannel.Cache = nil
	noChannel.UnionReady = workspace.UnionReady
	if _, err := noChannel.ContainerCreate(t.Context(), []byte(bind)); err == nil ||
		!strings.Contains(err.Error(), "write=back") {
		t.Fatalf("with no channel: err = %v, want the write mode named", err)
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
func TestOneDirectoryCannotHaveTwoModes(t *testing.T) {
	r, _ := cachedRewriter()

	_, err := r.ContainerCreate(t.Context(), []byte(`{"HostConfig":{"Binds":[`+
		`"/home/alice/project:/app:read=cached","/home/alice/project:/also:read=direct"]}}`))
	if err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("err = %v, want a refusal naming the repeat", err)
	}

	// The same directory asking for the same thing twice is ordinary, in
	// either spelling.
	r, volumes := cachedRewriter()
	if _, err := r.ContainerCreate(t.Context(), []byte(`{"HostConfig":{"Binds":[`+
		`"/home/alice/project:/app:read=cached","/home/alice/project:/also:read=cached,write=through"]}}`)); err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	if o := optionsFor(t, volumes); !strings.Contains(o, "actimeo=60") {
		t.Errorf("volume options = %q", o)
	}
}

// A word nobody else in the toolchain understands is refused where it is
// written, rather than forwarded to a daemon that will report something else.
func TestAMountModeThatIsNotOneIsRefused(t *testing.T) {
	r, _ := cachedRewriter()

	for _, value := range []string{"fast", "read=fast", "write=fast"} {
		_, err := r.ContainerCreate(t.Context(), []byte(`{"HostConfig":{"Mounts":[`+
			`{"Type":"bind","Source":"/home/alice/project","Target":"/app","Consistency":"`+value+`"}]}}`))
		if err == nil || !strings.Contains(err.Error(), "fast") {
			t.Errorf("%s: err = %v, want the word named", value, err)
		}
	}
}

// A single file leaves Binds for Mounts (ADR 0039), and the mode is consumed
// before that walk: fileMount refuses any option it does not know, so a word
// left in the list would make `-v file:/f:ro,read=cached` fail on the option
// rather than mount the file.
func TestASingleFileTakesAModeToo(t *testing.T) {
	r, sharer, volumes := newRewriter()
	r.Watching = true
	sharer.files = map[string]string{"/home/alice/app.conf": "app.conf"}

	out, err := r.ContainerCreate(t.Context(),
		[]byte(`{"HostConfig":{"Binds":["/home/alice/app.conf:/etc/app.conf:ro,read=cached"]}}`))
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

// A union holds actual copies, so the watcher is what keeps them honest: a
// cached copy of a file that changed here is the one way this mode could be
// wrong rather than merely slow.
func TestAUnionNeedsTheWatcherToo(t *testing.T) {
	r, _, _ := newRewriter()
	r.Cache = &fakeCache{}
	r.UnionReady = workspace.UnionReady

	_, err := r.ContainerCreate(t.Context(),
		[]byte(`{"HostConfig":{"Binds":["/home/alice/project:/app:read=cached,write=back"]}}`))
	if err == nil {
		t.Fatal("a cache was served with nothing to keep it honest")
	}
	if !strings.Contains(err.Error(), "watch") {
		t.Errorf("error = %v, want the setting named", err)
	}
}
