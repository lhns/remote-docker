package fswatch

import (
	"context"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/lhns/remote-docker/pkg/workspace"
)

type fakeSink struct {
	mu     sync.Mutex
	frames []workspace.NotifyFrame
	err    error
}

func (s *fakeSink) Send(_ context.Context, f workspace.NotifyFrame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.frames = append(s.frames, f)
	return nil
}

func (s *fakeSink) events() []workspace.FSEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []workspace.FSEvent
	for _, f := range s.frames {
		out = append(out, f.Events...)
	}
	return out
}

func (s *fakeSink) notices() []workspace.FSNotice {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []workspace.FSNotice
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

func startWatcher(t *testing.T, mode Mode, root string) (*Watcher, *fakeBackend, *fakeSink) {
	t.Helper()
	be := newFakeBackend()
	w, err := New(Options{
		Mode:     mode,
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
	w.Sync([]Share{{ExportPath: workspace.ExportCWD, LocalPath: root}})
	waitFor(t, "the initial walk", func() bool { return w.Stats().Watched > 0 })
	return w, be, sink
}

func TestWatcherReportsAWrite(t *testing.T) {
	root := mkdirs(t, t.TempDir(), "src")
	w, be, sink := startWatcher(t, ModePartial, root)

	be.events <- fsnotify.Event{Name: filepath.Join(root, "src", "a.go"), Op: fsnotify.Write}

	waitFor(t, "the write to arrive", func() bool { return len(sink.events()) > 0 })
	got := sink.events()[0]
	if got.Export != workspace.ExportCWD || got.Path != "/src/a.go" || got.Op != workspace.OpWrite {
		t.Errorf("reported %+v, want /src/a.go write on /cwd", got)
	}
	if w.Stats().Sent == 0 {
		t.Error("Stats did not count the sent event")
	}
}

// partial must never misrepresent a deletion as something else, so it sends
// nothing at all for one.
func TestPartialModeDropsRemovals(t *testing.T) {
	root := mkdirs(t, t.TempDir(), "src")
	_, be, sink := startWatcher(t, ModePartial, root)

	be.events <- fsnotify.Event{Name: filepath.Join(root, "src", "gone.go"), Op: fsnotify.Remove}
	be.events <- fsnotify.Event{Name: filepath.Join(root, "src", "kept.go"), Op: fsnotify.Write}

	waitFor(t, "the write to arrive", func() bool { return len(sink.events()) > 0 })
	// Give a removal every chance to arrive late before concluding it did not.
	time.Sleep(50 * time.Millisecond)

	for _, e := range sink.events() {
		if e.Op&(workspace.OpRemove|workspace.OpRename) != 0 {
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
	_, be, sink := startWatcher(t, ModeCoarse, root)

	be.events <- fsnotify.Event{Name: filepath.Join(root, "src", "gone.go"), Op: fsnotify.Remove}

	waitFor(t, "the removal to arrive", func() bool {
		for _, e := range sink.events() {
			if e.Path == "/src/gone.go" && e.Op&workspace.OpRemove != 0 {
				return true
			}
		}
		return false
	})
}

func TestOffModeWatchesNothing(t *testing.T) {
	root := mkdirs(t, t.TempDir(), "src")
	w, err := New(Options{Mode: ModeOff, goos: runtime.GOOS,
		newBackend: func() (backend, error) { t.Fatal("off mode created a backend"); return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()

	sink := &fakeSink{}
	w.SetSink(sink)
	w.Sync([]Share{{ExportPath: workspace.ExportCWD, LocalPath: root}})
	time.Sleep(50 * time.Millisecond)

	if s := w.Stats(); s.Watched != 0 || s.Sent != 0 {
		t.Errorf("off mode did work: %+v", s)
	}
	if len(sink.events()) != 0 {
		t.Errorf("off mode sent events: %+v", sink.events())
	}
}

// Losing the connection must not disturb the watches -- re-walking a large
// tree on every idle reconnect would cost more than the connection did -- but
// what happened meanwhile must be admitted rather than quietly forgotten.
func TestReconnectAnnouncesWhatWasMissed(t *testing.T) {
	root := mkdirs(t, t.TempDir(), "src")
	w, be, first := startWatcher(t, ModePartial, root)
	watched := w.Stats().Watched

	w.ClearSink()
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
	w, be, sink := startWatcher(t, ModePartial, root)

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
	_, be, sink := startWatcher(t, ModePartial, root)

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
	_, be, sink := startWatcher(t, ModeCoarse, root)

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
	w, _, _ := startWatcher(t, ModePartial, root)
	if err := w.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestParseMode(t *testing.T) {
	tests := map[string]Mode{
		"": ModeOff, "off": ModeOff, "OFF": ModeOff, "false": ModeOff, "0": ModeOff,
		"partial": ModePartial, "on": ModePartial, "true": ModePartial, " partial ": ModePartial,
		"coarse": ModeCoarse, "best-effort": ModeCoarse,
	}
	for in, want := range tests {
		got, err := ParseMode(in)
		if err != nil {
			t.Errorf("ParseMode(%q): %v", in, err)
		}
		if got != want {
			t.Errorf("ParseMode(%q) = %v, want %v", in, got, want)
		}
	}
	if _, err := ParseMode("sometimes"); err == nil {
		t.Error("ParseMode accepted an unknown mode")
	}
}

func TestModeString(t *testing.T) {
	for mode, want := range map[Mode]string{ModeOff: "off", ModePartial: "partial", ModeCoarse: "coarse"} {
		if got := mode.String(); got != want {
			t.Errorf("Mode(%d).String() = %q, want %q", int(mode), got, want)
		}
		// Round trips, so a value printed in `status` can be given back as
		// configuration.
		if back, err := ParseMode(want); err != nil || back != mode {
			t.Errorf("ParseMode(%q) = %v, %v; want %v", want, back, err, mode)
		}
	}
}
