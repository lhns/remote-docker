package replay

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/lhns/remote-docker/core/notify"
	"github.com/lhns/remote-docker/core/workspace"
)

type fakePoker struct {
	mu    sync.Mutex
	poked []string
	dirs  map[string]bool
	err   error
}

func newFakePoker() *fakePoker { return &fakePoker{dirs: map[string]bool{}} }

func (p *fakePoker) Poke(path string, isDir bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.poked = append(p.poked, path)
	if isDir {
		p.dirs[path] = true
	}
	return p.err
}

func (p *fakePoker) sorted() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := slices.Clone(p.poked)
	sort.Strings(out)
	return out
}

func (p *fakePoker) count(path string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, q := range p.poked {
		if q == path {
			n++
		}
	}
	return n
}

type fakeVolumes struct {
	mu     sync.Mutex
	byName map[string]string
	calls  int
}

func (v *fakeVolumes) Mountpoint(_ context.Context, volume string) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls++
	if mp, ok := v.byName[volume]; ok {
		return mp, nil
	}
	return "", errors.New("no such volume")
}

const cwdVolume = "rd-cwd"

func newReplayer(mounts map[string]string) (*Replayer, *fakePoker, *fakeVolumes) {
	poker := newFakePoker()
	vols := &fakeVolumes{byName: mounts}
	return &Replayer{Volumes: vols, Poker: poker}, poker, vols
}

// feed runs frames through Serve and returns what the agent wrote back.
func feed(t *testing.T, r *Replayer, frames ...notify.Frame) string {
	t.Helper()
	var in strings.Builder
	for _, f := range frames {
		b, err := json.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
		in.WriteString(string(b) + "\n")
	}
	var out strings.Builder
	rw := struct {
		io.Reader
		io.Writer
	}{strings.NewReader(in.String()), &out}
	if err := r.Serve(context.Background(), rw); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	return out.String()
}

// The greeting is what tells a client that this agent understands the command
// at all. An older agent falls through to the generic exec path, runs
// `sh -c "workspace-notify"` and exits 127, so without a hello the client
// cannot distinguish a working channel from a missing one.
func TestServeGreetsFirst(t *testing.T) {
	r, _, _ := newReplayer(map[string]string{cwdVolume: "/mnt"})
	out := feed(t, r)

	line, _, _ := strings.Cut(out, "\n")
	var frame notify.Frame
	if err := json.Unmarshal([]byte(line), &frame); err != nil {
		t.Fatalf("the first line is not a frame: %q (%v)", line, err)
	}
	if frame.Hello == nil {
		t.Fatalf("the first frame is not a hello: %q", line)
	}
	if frame.Hello.Version != notify.Version {
		t.Errorf("announced version %d, want %d", frame.Hello.Version, notify.Version)
	}
}

func TestReplaysAWrite(t *testing.T) {
	root := filepath.FromSlash("/mnt/cwd")
	r, poker, _ := newReplayer(map[string]string{cwdVolume: root})

	feed(t, r, notify.Frame{Events: []notify.Event{
		{Export: workspace.ExportCWD, Path: "/src/a.go", Op: notify.OpWrite},
	}})

	want := filepath.Join(root, "src", "a.go")
	if got := poker.sorted(); len(got) != 1 || got[0] != want {
		t.Errorf("poked %v, want exactly [%s]", got, want)
	}
}

// A creation pokes the file, so a watcher keyed on the file notices, and the
// parent, so one keyed on the directory rescans and finds it.
func TestCreatePokesTheFileAndItsParent(t *testing.T) {
	root := filepath.FromSlash("/mnt/cwd")
	r, poker, _ := newReplayer(map[string]string{cwdVolume: root})

	feed(t, r, notify.Frame{Events: []notify.Event{
		{Export: workspace.ExportCWD, Path: "/src/new.go", Op: notify.OpCreate},
	}})

	got := poker.sorted()
	want := []string{filepath.Join(root, "src"), filepath.Join(root, "src", "new.go")}
	if !slices.Equal(got, want) {
		t.Errorf("poked %v, want %v", got, want)
	}
	if !poker.dirs[filepath.Join(root, "src")] {
		t.Error("the parent was not poked as a directory; O_WRONLY on one is EISDIR")
	}
}

// A removal has nothing to touch: the file is gone, and unlink of an
// already-gone name fails before the kernel generates anything. Only the
// parent can be said, and the vanished path must NOT be poked: doing so with
// O_CREAT semantics anywhere would recreate it.
func TestRemovePokesOnlyTheParent(t *testing.T) {
	root := filepath.FromSlash("/mnt/cwd")
	r, poker, _ := newReplayer(map[string]string{cwdVolume: root})

	feed(t, r, notify.Frame{Events: []notify.Event{
		{Export: workspace.ExportCWD, Path: "/src/gone.go", Op: notify.OpRemove},
	}})

	got := poker.sorted()
	if len(got) != 1 || got[0] != filepath.Join(root, "src") {
		t.Errorf("poked %v, want only the parent directory", got)
	}
	for _, p := range got {
		if strings.HasSuffix(p, "gone.go") {
			t.Errorf("poked the removed path %s; replay must never bring a file back", p)
		}
	}
}

// A save touching many files in one directory must not become many identical
// directory pokes.
func TestParentDirectoryIsPokedOncePerFrame(t *testing.T) {
	root := filepath.FromSlash("/mnt/cwd")
	r, poker, _ := newReplayer(map[string]string{cwdVolume: root})

	var events []notify.Event
	for _, n := range []string{"a.go", "b.go", "c.go", "d.go"} {
		events = append(events, notify.Event{
			Export: workspace.ExportCWD, Path: "/src/" + n, Op: notify.OpCreate,
		})
	}
	feed(t, r, notify.Frame{Events: events})

	if got := poker.count(filepath.Join(root, "src")); got != 1 {
		t.Errorf("poked the parent %d times, want 1", got)
	}
}

// The security-critical test. This stream tells a root process which path to
// touch, so the agent validates independently of the client.
func TestRefusesMalformedEvents(t *testing.T) {
	root := filepath.FromSlash("/mnt/cwd")

	bad := []notify.Event{
		{Export: workspace.ExportCWD, Path: "/../../etc/shadow", Op: notify.OpWrite},
		{Export: workspace.ExportCWD, Path: "/a/../../../etc/passwd", Op: notify.OpWrite},
		{Export: workspace.ExportCWD, Path: "relative", Op: notify.OpWrite},
		{Export: workspace.ExportCWD, Path: `/a\b`, Op: notify.OpWrite},
		{Export: "/etc", Path: "/a", Op: notify.OpWrite},
		{Export: workspace.ExportCWD, Path: "/a", Op: 0},
		{Export: workspace.ExportCWD, Path: "/a", Op: 1 << 7},
	}

	for _, e := range bad {
		r, poker, _ := newReplayer(map[string]string{cwdVolume: root})
		feed(t, r, notify.Frame{Events: []notify.Event{e}})
		if got := poker.sorted(); len(got) != 0 {
			t.Errorf("event %+v was refused by neither end; it poked %v", e, got)
		}
	}
}

// resolve is the last line of defence and must hold even if validation is
// somehow bypassed.
func TestResolveRefusesEscapes(t *testing.T) {
	root := filepath.FromSlash("/mnt/cwd")
	for _, share := range []string{"/../etc", "/../../etc/shadow", "/a/../../.."} {
		if got, ok := resolve(root, share); ok && !strings.HasPrefix(got, root) {
			t.Errorf("resolve(%q, %q) = %q, which escapes the root", root, share, got)
		}
	}
	// The ordinary cases still work.
	if got, ok := resolve(root, "/a/b.go"); !ok || got != filepath.Join(root, "a", "b.go") {
		t.Errorf("resolve of an ordinary path = %q, %v", got, ok)
	}
	if got, ok := resolve(root, "/"); !ok || got != root {
		t.Errorf("resolve of the share root = %q, %v; want %q", got, ok, root)
	}
}

// A share no container has mounted yet is ordinary: the user shared a
// directory and has not started anything using it. It must not become a stream
// of docker calls.
func TestUnknownVolumeIsCachedAndSilent(t *testing.T) {
	r, poker, vols := newReplayer(map[string]string{})

	var events []notify.Event
	for range 20 {
		events = append(events, notify.Event{
			Export: workspace.ExportCWD, Path: "/a.go", Op: notify.OpWrite,
		})
	}
	feed(t, r, notify.Frame{Events: events})

	if got := poker.sorted(); len(got) != 0 {
		t.Errorf("poked %v with no volume mounted", got)
	}
	if vols.calls > 1 {
		t.Errorf("asked docker %d times for one unresolvable volume", vols.calls)
	}
}

func TestMountpointIsCached(t *testing.T) {
	root := filepath.FromSlash("/mnt/cwd")
	r, _, vols := newReplayer(map[string]string{cwdVolume: root})

	for range 10 {
		feed(t, r, notify.Frame{Events: []notify.Event{
			{Export: workspace.ExportCWD, Path: "/a.go", Op: notify.OpWrite},
		}})
	}
	if vols.calls != 1 {
		t.Errorf("asked docker %d times, want 1", vols.calls)
	}
}

// A notice means the client's own picture is incomplete. Replaying events we
// never received is not on offer, so the directory it names is poked instead.
func TestNoticePokesTheNamedDirectory(t *testing.T) {
	root := filepath.FromSlash("/mnt/cwd")
	r, poker, _ := newReplayer(map[string]string{cwdVolume: root})

	feed(t, r, notify.Frame{Notice: &notify.Notice{
		Export: workspace.ExportCWD, Path: "/src/deep", Dropped: 900, Reason: "overflow",
	}})

	got := poker.sorted()
	want := filepath.Join(root, "src", "deep")
	if len(got) != 1 || got[0] != want {
		t.Errorf("poked %v, want [%s]", got, want)
	}
	if !poker.dirs[want] {
		t.Error("a notice poked a path as a file, not a directory")
	}
}

// A malformed line is a client bug, not a reason to tear down a working
// session: the next frame is very likely fine.
func TestMalformedFrameDoesNotEndTheStream(t *testing.T) {
	root := filepath.FromSlash("/mnt/cwd")
	r, poker, _ := newReplayer(map[string]string{cwdVolume: root})

	good, _ := json.Marshal(notify.Frame{Events: []notify.Event{
		{Export: workspace.ExportCWD, Path: "/after.go", Op: notify.OpWrite},
	}})
	in := "{not json\n\n" + string(good) + "\n"

	var out strings.Builder
	rw := struct {
		io.Reader
		io.Writer
	}{strings.NewReader(in), &out}
	if err := r.Serve(context.Background(), rw); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if got := poker.count(filepath.Join(root, "after.go")); got != 1 {
		t.Errorf("the frame after a malformed one was not replayed (poked %d times)", got)
	}
}

// One frame can carry events for several shares, and each must be resolved
// against its own mountpoint.
func TestEventsSpanningExports(t *testing.T) {
	const otherExport = "/m/0123456789abcdef"
	cwdRoot := filepath.FromSlash("/mnt/cwd")
	otherRoot := filepath.FromSlash("/mnt/other")
	r, poker, _ := newReplayer(map[string]string{
		cwdVolume:             cwdRoot,
		"rd-0123456789abcdef": otherRoot,
	})

	feed(t, r, notify.Frame{Events: []notify.Event{
		{Export: workspace.ExportCWD, Path: "/a/one.go", Op: notify.OpCreate},
		{Export: otherExport, Path: "/b/two.go", Op: notify.OpCreate},
	}})

	got := poker.sorted()
	want := []string{
		filepath.Join(cwdRoot, "a"),
		filepath.Join(cwdRoot, "a", "one.go"),
		filepath.Join(otherRoot, "b"),
		filepath.Join(otherRoot, "b", "two.go"),
	}
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Errorf("poked %v, want %v -- each export resolves against its own mountpoint", got, want)
	}
}

func TestParentDir(t *testing.T) {
	tests := map[string]string{
		"/a/b.go": "/a",
		"/a.go":   "/",
		"/":       "/",
		"/a/b/c":  "/a/b",
	}
	for in, want := range tests {
		if got := parentDir(in); got != want {
			t.Errorf("parentDir(%q) = %q, want %q", in, got, want)
		}
	}
}
