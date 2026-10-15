package fswatch

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/lhns/remote-docker/core/notify"
	"github.com/lhns/remote-docker/core/workspace"
)

type fakeSink struct {
	mu     sync.Mutex
	frames []notify.Frame
	err    error
}

func (s *fakeSink) Send(_ context.Context, f notify.Frame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.frames = append(s.frames, f)
	return nil
}

func (s *fakeSink) events() []notify.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []notify.Event
	for _, f := range s.frames {
		out = append(out, f.Events...)
	}
	return out
}

func (s *fakeSink) notices() []notify.Notice {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []notify.Notice
	for _, f := range s.frames {
		if f.Notice != nil {
			out = append(out, *f.Notice)
		}
	}
	return out
}

// waitFor polls until cond holds. The watcher is goroutine-driven, so a test
// waits on the observable outcome rather than on a sleep long enough to be
// flaky on a loaded runner.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func startWatcher(t *testing.T, mode workspace.Watch, root string) (*Watcher, *fakeBackend, *fakeSink) {
	t.Helper()
	be := newFakeBackend()
	w, err := New(Options{
		Budget:   100,
		Debounce: time.Millisecond,
		MaxDelay: 5 * time.Millisecond,
		goos:     runtime.GOOS,
		newBackend: func() (backend, error) {
			return be, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })

	sink := &fakeSink{}
	w.SetSink(sink)
	w.Sync([]Share{{ExportPath: "/m/1111111111111111", LocalPath: root, Mode: mode}})
	waitFor(t, "the initial walk", func() bool { return w.Stats().Watched > 0 })
	return w, be, sink
}

// recordingObserver counts the events it was told of, per export.
type recordingObserver struct {
	mu     sync.Mutex
	events []notify.Event
}

func (o *recordingObserver) Observe(e notify.Event) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, e)
}

func (o *recordingObserver) Lost(notify.Notice) {}

func (o *recordingObserver) count(export string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := 0
	for _, e := range o.events {
		if e.Export == export {
			n++
		}
	}
	return n
}

// lostObserver records what the watcher says it missed.
type lostObserver struct {
	mu   sync.Mutex
	lost []notify.Notice
}

func (o *lostObserver) Observe(notify.Event) {}

func (o *lostObserver) Lost(n notify.Notice) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.lost = append(o.lost, n)
}

func (o *lostObserver) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.lost)
}

// A kernel queue overflow loses events nobody can name, deletions included, and
// a cache answers that with a reconcile (Observer.Lost). It was logged and
// nothing else, so a deletion lost to it stayed shadowed by a cached copy.
func TestWatcherReportsAKernelOverflow(t *testing.T) {
	root := mkdirs(t, t.TempDir(), "src")
	w, be, sink := startWatcher(t, workspace.WatchPartial, root)
	obs := &lostObserver{}
	w.SetObserver(obs)

	be.errors <- fsnotify.ErrEventOverflow

	waitFor(t, "the overflow to reach the observer and the agent", func() bool {
		return obs.count() > 0 && len(sink.notices()) > 0
	})
	if n := sink.notices()[0]; n.Export != "/m/1111111111111111" || n.Path != "/" {
		t.Errorf("notice %+v, want the whole of %s", n, "/m/1111111111111111")
	}
}

func TestWatcherReportsAWrite(t *testing.T) {
	root := mkdirs(t, t.TempDir(), "src")
	w, be, sink := startWatcher(t, workspace.WatchPartial, root)

	be.events <- fsnotify.Event{Name: filepath.Join(root, "src", "a.go"), Op: fsnotify.Write}

	waitFor(t, "the write to arrive", func() bool { return len(sink.events()) > 0 })
	got := sink.events()[0]
	if got.Export != "/m/1111111111111111" || got.Path != "/src/a.go" || got.Op != notify.OpWrite {
		t.Errorf("reported %+v, want /src/a.go write on the share", got)
	}
	if w.Stats().Sent == 0 {
		t.Error("Stats did not count the sent event")
	}
}

// partial must never misrepresent a deletion as something else, so it sends
// nothing at all for one.
func TestPartialModeDropsRemovals(t *testing.T) {
	root := mkdirs(t, t.TempDir(), "src")
	_, be, sink := startWatcher(t, workspace.WatchPartial, root)

	be.events <- fsnotify.Event{Name: filepath.Join(root, "src", "gone.go"), Op: fsnotify.Remove}
	be.events <- fsnotify.Event{Name: filepath.Join(root, "src", "kept.go"), Op: fsnotify.Write}

	waitFor(t, "the write to arrive", func() bool { return len(sink.events()) > 0 })
	// Give a removal every chance to arrive late before concluding it did not.
	time.Sleep(50 * time.Millisecond)

	for _, e := range sink.events() {
		if e.Op&(notify.OpRemove|notify.OpRename) != 0 {
			t.Errorf("partial mode sent %+v; it cannot replay a removal faithfully", e)
		}
		if e.Path == "/src/gone.go" {
			t.Errorf("partial mode reported a removed path: %+v", e)
		}
	}
}

// coarse accepts an inaccurate event kind in exchange for the change being
// noticed at all. That is the user's trade, so the event must actually be sent.
func TestCoarseModeSendsRemovals(t *testing.T) {
	root := mkdirs(t, t.TempDir(), "src")
	_, be, sink := startWatcher(t, workspace.WatchCoarse, root)

	be.events <- fsnotify.Event{Name: filepath.Join(root, "src", "gone.go"), Op: fsnotify.Remove}

	waitFor(t, "the removal to arrive", func() bool {
		for _, e := range sink.events() {
			if e.Path == "/src/gone.go" && e.Op&notify.OpRemove != 0 {
				return true
			}
		}
		return false
	})
}

// A share whose mode is off is not watched, whatever the watcher is doing for
// the others.
func TestAShareInOffModeIsNotWatched(t *testing.T) {
	watched := mkdirs(t, t.TempDir(), "src")
	off := mkdirs(t, t.TempDir(), "src")

	be := newFakeBackend()
	w, err := New(Options{Budget: 100, Debounce: time.Millisecond, MaxDelay: 5 * time.Millisecond, goos: runtime.GOOS,
		newBackend: func() (backend, error) { return be, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	sink := &fakeSink{}
	w.SetSink(sink)

	w.Sync([]Share{
		{ExportPath: "/m/1111111111111111", LocalPath: watched, Mode: workspace.WatchPartial},
		{ExportPath: "/m/2222222222222222", LocalPath: off, Mode: workspace.WatchOff},
	})
	waitFor(t, "the walk", func() bool { return w.Stats().Watched > 0 })

	be.mu.Lock()
	added := append([]string(nil), be.added...)
	be.mu.Unlock()
	for _, dir := range added {
		if strings.HasPrefix(dir, off) {
			t.Errorf("an off share was watched: %s", dir)
		}
	}
	be.events <- fsnotify.Event{Name: filepath.Join(off, "src", "a.go"), Op: fsnotify.Write}
	be.events <- fsnotify.Event{Name: filepath.Join(watched, "src", "b.go"), Op: fsnotify.Write}
	waitFor(t, "the watched share's write", func() bool { return len(sink.events()) > 0 })
	time.Sleep(50 * time.Millisecond)
	for _, e := range sink.events() {
		if e.Export != "/m/1111111111111111" {
			t.Errorf("an event for the off share was sent: %+v", e)
		}
	}
}

// One watcher serves shares that disagree: a deletion is stripped from the
// partial one and sent for the coarse one, and the observer (a cache) is
// told of both before any mode strips anything.
func TestSharesInOneWatcherKeepTheirOwnModes(t *testing.T) {
	partial := mkdirs(t, t.TempDir(), "src")
	coarse := mkdirs(t, t.TempDir(), "src")

	be := newFakeBackend()
	w, err := New(Options{Budget: 100, Debounce: time.Millisecond, MaxDelay: 5 * time.Millisecond, goos: runtime.GOOS,
		newBackend: func() (backend, error) { return be, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	sink := &fakeSink{}
	w.SetSink(sink)
	seen := &recordingObserver{}
	w.SetObserver(seen)

	w.Sync([]Share{
		{ExportPath: "/m/1111111111111111", LocalPath: partial, Mode: workspace.WatchPartial},
		{ExportPath: "/m/2222222222222222", LocalPath: coarse, Mode: workspace.WatchCoarse},
	})
	waitFor(t, "the walk", func() bool { return w.Stats().Watched >= 4 })

	be.events <- fsnotify.Event{Name: filepath.Join(partial, "src", "gone.go"), Op: fsnotify.Remove}
	be.events <- fsnotify.Event{Name: filepath.Join(coarse, "src", "gone.go"), Op: fsnotify.Remove}

	waitFor(t, "the coarse removal", func() bool {
		for _, e := range sink.events() {
			if e.Export == "/m/2222222222222222" && e.Op&notify.OpRemove != 0 {
				return true
			}
		}
		return false
	})
	time.Sleep(50 * time.Millisecond)
	for _, e := range sink.events() {
		if e.Export == "/m/1111111111111111" {
			t.Errorf("the partial share sent %+v", e)
		}
	}
	if got := seen.count("/m/1111111111111111"); got != 1 {
		t.Errorf("the observer saw %d events for the partial share, want its deletion", got)
	}
}

// Nested shares can carry different modes, so an event belongs to the DEEPEST
// one whatever order they registered in.
func TestNestedSharesResolveToTheDeepest(t *testing.T) {
	outer := mkdirs(t, t.TempDir(), "inner")
	inner := filepath.Join(outer, "inner")

	for _, order := range [][]Share{
		{{ExportPath: "/m/1111111111111111", LocalPath: outer, Mode: workspace.WatchPartial}, {ExportPath: "/m/2222222222222222", LocalPath: inner, Mode: workspace.WatchCoarse}},
		{{ExportPath: "/m/2222222222222222", LocalPath: inner, Mode: workspace.WatchCoarse}, {ExportPath: "/m/1111111111111111", LocalPath: outer, Mode: workspace.WatchPartial}},
	} {
		tr := newTree(runtime.GOOS, newFakeBackend(), 100, nil, nil)
		tr.sync(order)
		got := tr.rootsFor(filepath.Join(inner, "x.go"))
		if len(got) != 1 || got[0].root.export != "/m/2222222222222222" || got[0].rel != "/x.go" {
			t.Errorf("order %v: rootsFor = %+v, want the inner share", order, got)
		}
	}
}

// Losing the connection must not disturb the watches: re-walking a large
// tree on every idle reconnect would cost more than the connection did, but
// what happened meanwhile must be admitted rather than quietly forgotten.
func TestReconnectAnnouncesWhatWasMissed(t *testing.T) {
	root := mkdirs(t, t.TempDir(), "src")
	w, be, first := startWatcher(t, workspace.WatchPartial, root)
	watched := w.Stats().Watched

	w.SetSink(nil)
	be.events <- fsnotify.Event{Name: filepath.Join(root, "src", "while-away.go"), Op: fsnotify.Write}
	waitFor(t, "the event to be dropped", func() bool { return w.Stats().Dropped > 0 })

	if w.Stats().Watched != watched {
		t.Errorf("disconnecting changed the watched set: %d -> %d", watched, w.Stats().Watched)
	}
	if len(first.events()) != 0 {
		t.Errorf("events reached a detached sink: %+v", first.events())
	}

	second := &fakeSink{}
	w.SetSink(second)
	be.events <- fsnotify.Event{Name: filepath.Join(root, "src", "after.go"), Op: fsnotify.Write}

	waitFor(t, "the disconnected notice", func() bool {
		for _, n := range second.notices() {
			if n.Reason == "disconnected" && n.Dropped > 0 {
				return true
			}
		}
		return false
	})
}

// A directory created after the walk must be watched, and re-walked: anything
// inside it between the mkdir and the watch landing produced no event at all.
func TestNewDirectoryIsWatchedAndItsContentsReported(t *testing.T) {
	root := mkdirs(t, t.TempDir(), "src")
	w, be, sink := startWatcher(t, workspace.WatchPartial, root)

	// The directory and its contents exist before we are told about it, which
	// is the race as it actually happens.
	late := mkdirs(t, root, "late", "late/inner")
	if err := writeFile(filepath.Join(late, "late", "inner", "x.go")); err != nil {
		t.Fatal(err)
	}
	be.events <- fsnotify.Event{Name: filepath.Join(root, "late"), Op: fsnotify.Create}

	waitFor(t, "the contents of the new directory", func() bool {
		for _, e := range sink.events() {
			if e.Path == "/late/inner/x.go" {
				return true
			}
		}
		return false
	})
	waitFor(t, "the new directory to be watched", func() bool {
		return be.addedSet()[filepath.Join(root, "late", "inner")]
	})
	if w.Stats().Watched < 4 {
		t.Errorf("watched %d directories, want at least 4", w.Stats().Watched)
	}
}

// An event under no share is ordinary during a resync and must not panic or be
// reported against the wrong export.
func TestEventOutsideEveryShareIsIgnored(t *testing.T) {
	root := mkdirs(t, t.TempDir(), "src")
	_, be, sink := startWatcher(t, workspace.WatchPartial, root)

	be.events <- fsnotify.Event{Name: filepath.Join(t.TempDir(), "elsewhere.go"), Op: fsnotify.Write}
	be.events <- fsnotify.Event{Name: filepath.Join(root, "marker.go"), Op: fsnotify.Write}

	waitFor(t, "the in-share event", func() bool { return len(sink.events()) > 0 })
	for _, e := range sink.events() {
		if e.Path != "/marker.go" {
			t.Errorf("reported an out-of-share path: %+v", e)
		}
	}
}

// Everything reaching the sink must pass the validation the agent applies, or
// the agent refuses a whole frame and the failure appears as "notifications do
// not work" with nothing in either log explaining it.
func TestEverythingSentIsValid(t *testing.T) {
	root := mkdirs(t, t.TempDir(), "src", "src/deep")
	_, be, sink := startWatcher(t, workspace.WatchCoarse, root)

	for _, name := range []string{"a.go", "src/b.go", "src/deep/c.go", ".env", "sp ace.txt"} {
		be.events <- fsnotify.Event{Name: filepath.Join(root, filepath.FromSlash(name)), Op: fsnotify.Write}
	}

	waitFor(t, "all events", func() bool { return len(sink.events()) >= 5 })
	for _, e := range sink.events() {
		if err := e.Validate(); err != nil {
			t.Errorf("sent %+v, which the agent would reject: %v", e, err)
		}
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	root := t.TempDir()
	w, _, _ := startWatcher(t, workspace.WatchPartial, root)
	if err := w.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// A nil exclude list means the defaults; an explicit list, empty included, is
// applied as it stands.
func TestExcludesOr(t *testing.T) {
	if got := ExcludesOr(nil); len(got) != len(DefaultExcludes) {
		t.Errorf("ExcludesOr(nil) = %v, want DefaultExcludes", got)
	}
	if got := ExcludesOr([]string{}); len(got) != 0 {
		t.Errorf("ExcludesOr([]) = %v, want an empty list", got)
	}
}

// One directory mounted two ways: an event reaches both shares, each by its own
// mode. The cache is told of both before any mode strips anything.
func TestTwoSharesOnOnePathEachGetTheEvent(t *testing.T) {
	dir := mkdirs(t, t.TempDir(), "src")

	be := newFakeBackend()
	w, err := New(Options{Budget: 100, Debounce: time.Millisecond, MaxDelay: 5 * time.Millisecond, goos: runtime.GOOS,
		newBackend: func() (backend, error) { return be, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	sink := &fakeSink{}
	w.SetSink(sink)
	seen := &recordingObserver{}
	w.SetObserver(seen)

	w.Sync([]Share{
		{ExportPath: "/m/1111111111111111", LocalPath: dir, Mode: workspace.WatchPartial},
		{ExportPath: "/m/2222222222222222", LocalPath: dir, Mode: workspace.WatchCoarse},
	})
	waitFor(t, "the walk", func() bool { return w.Stats().Watched >= 2 })

	be.events <- fsnotify.Event{Name: filepath.Join(dir, "src", "a.go"), Op: fsnotify.Write}
	be.events <- fsnotify.Event{Name: filepath.Join(dir, "src", "gone.go"), Op: fsnotify.Remove}

	has := func(export string, op notify.Op) bool {
		for _, e := range sink.events() {
			if e.Export == export && e.Op&op != 0 {
				return true
			}
		}
		return false
	}
	waitFor(t, "both shares' writes and the coarse removal", func() bool {
		return has("/m/1111111111111111", notify.OpWrite) && has("/m/2222222222222222", notify.OpWrite) &&
			has("/m/2222222222222222", notify.OpRemove)
	})
	time.Sleep(50 * time.Millisecond)
	if has("/m/1111111111111111", notify.OpRemove) {
		t.Error("the partial share was sent a deletion")
	}
	for _, export := range []string{"/m/1111111111111111", "/m/2222222222222222"} {
		if got := seen.count(export); got != 2 {
			t.Errorf("the observer saw %d events for %s, want both", got, export)
		}
	}
}
