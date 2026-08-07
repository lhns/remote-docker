package fswatch

import (
	"testing"
	"time"

	"github.com/lhns/remote-docker/pkg/workspace"
)

// base is an arbitrary fixed instant. The coalescer takes the time from its
// caller and owns no clock, so every test here is exact rather than timed --
// no sleeps, no synctest, and nothing that can go flaky on a loaded runner.
var base = time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

func ev(p string, op workspace.FSOp) workspace.FSEvent {
	return workspace.FSEvent{Export: workspace.ExportCWD, Path: p, Op: op}
}

// One editor save produces three to five raw events for the same file. They
// must arrive at the agent as one poke, or a save costs five syscalls inside
// the workspace and the channel becomes the load it was meant to avoid.
func TestCoalescesRepeatedEventsForOnePath(t *testing.T) {
	c := newCoalescer(40*time.Millisecond, 250*time.Millisecond, 0)

	c.add(base, ev("/main.go", workspace.OpWrite))
	c.add(base.Add(5*time.Millisecond), ev("/main.go", workspace.OpWrite))
	c.add(base.Add(10*time.Millisecond), ev("/main.go", workspace.OpAttrib))

	if events, _ := c.flush(base.Add(20 * time.Millisecond)); len(events) != 0 {
		t.Fatalf("flushed %d events while still inside the debounce window", len(events))
	}

	events, notices := c.flush(base.Add(60 * time.Millisecond))
	if len(notices) != 0 {
		t.Errorf("unexpected notices: %+v", notices)
	}
	if len(events) != 1 {
		t.Fatalf("flushed %d events, want 1: %+v", len(events), events)
	}
	if got, want := events[0].Op, workspace.OpWrite|workspace.OpAttrib; got != want {
		t.Errorf("merged op = %v, want %v", got, want)
	}
}

// A file being appended to continuously is never quiet for a whole debounce
// interval. Without the maximum delay it would be held back forever, which is
// the same silent nothing-happens this feature exists to remove.
func TestMaxDelayForcesAFlush(t *testing.T) {
	c := newCoalescer(40*time.Millisecond, 250*time.Millisecond, 0)

	at := base
	for range 20 {
		c.add(at, ev("/build.log", workspace.OpWrite))
		at = at.Add(20 * time.Millisecond) // always inside the debounce window
	}

	events, _ := c.flush(at)
	if len(events) != 1 {
		t.Fatalf("a continuously-touched path was never reported: got %d events", len(events))
	}
}

// Both orders occur and mean different things, so merging by OR would report
// both and leave the agent guessing.
func TestMergeOps(t *testing.T) {
	tests := []struct {
		name string
		seq  []workspace.FSOp
		want workspace.FSOp
	}{
		{"write then write", []workspace.FSOp{workspace.OpWrite, workspace.OpWrite}, workspace.OpWrite},
		{"create then write", []workspace.FSOp{workspace.OpCreate, workspace.OpWrite}, workspace.OpCreate | workspace.OpWrite},
		// A temporary file: it was made and it is gone, and the net truth is
		// that it is gone.
		{"create then remove", []workspace.FSOp{workspace.OpCreate, workspace.OpRemove}, workspace.OpRemove},
		{"write then remove", []workspace.FSOp{workspace.OpWrite, workspace.OpRemove}, workspace.OpRemove},
		// The atomic save every editor performs: the original is unlinked and
		// a new file takes its place. The net truth is that it is there.
		{"remove then create", []workspace.FSOp{workspace.OpRemove, workspace.OpCreate}, workspace.OpCreate},
		{"rename then create", []workspace.FSOp{workspace.OpRename, workspace.OpCreate}, workspace.OpCreate},
		{"remove then create then write", []workspace.FSOp{workspace.OpRemove, workspace.OpCreate, workspace.OpWrite}, workspace.OpCreate | workspace.OpWrite},
		{"write then rename", []workspace.FSOp{workspace.OpWrite, workspace.OpRename}, workspace.OpRename},
		{"attrib is additive", []workspace.FSOp{workspace.OpWrite, workspace.OpAttrib}, workspace.OpWrite | workspace.OpAttrib},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var have workspace.FSOp
			for i, op := range tt.seq {
				if i == 0 {
					have = op
					continue
				}
				have = mergeOps(have, op)
			}
			if have != tt.want {
				t.Errorf("merge %v = %v, want %v", tt.seq, have, tt.want)
			}
		})
	}
}

// Flushing in arrival order preserves the only ordering that matters: a
// directory's creation necessarily arrived before its children's, so replaying
// in this order never asks the agent to touch a path whose parent it has not
// heard of.
func TestFlushIsInArrivalOrder(t *testing.T) {
	c := newCoalescer(10*time.Millisecond, time.Second, 0)

	c.add(base, ev("/src", workspace.OpCreate))
	c.add(base.Add(time.Millisecond), ev("/src/a.go", workspace.OpCreate))
	c.add(base.Add(2*time.Millisecond), ev("/src/b.go", workspace.OpCreate))
	// A later touch of the FIRST path must not move it to the back.
	c.add(base.Add(3*time.Millisecond), ev("/src", workspace.OpAttrib))

	events, _ := c.flush(base.Add(time.Second))
	var got []string
	for _, e := range events {
		got = append(got, e.Path)
	}
	want := []string{"/src", "/src/a.go", "/src/b.go"}
	if len(got) != len(want) {
		t.Fatalf("flushed %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("flushed %v, want %v", got, want)
		}
	}
}

// Over capacity, individual paths stop being remembered -- that is what being
// over capacity means -- but the loss is never silent.
func TestPendingCapProducesANotice(t *testing.T) {
	c := newCoalescer(10*time.Millisecond, time.Second, 2)

	c.add(base, ev("/a/one.go", workspace.OpWrite))
	c.add(base, ev("/a/two.go", workspace.OpWrite))
	c.add(base, ev("/a/b/three.go", workspace.OpWrite)) // dropped
	c.add(base, ev("/a/c/four.go", workspace.OpWrite))  // dropped

	events, notices := c.flush(base.Add(time.Second))
	if len(events) != 2 {
		t.Errorf("kept %d events, want 2", len(events))
	}
	if len(notices) != 1 {
		t.Fatalf("got %d notices, want 1", len(notices))
	}
	n := notices[0]
	if n.Dropped != 2 {
		t.Errorf("notice reports %d dropped, want 2", n.Dropped)
	}
	// The deepest directory covering both losses -- /a/b and /a/c -- is /a.
	if n.Path != "/a" {
		t.Errorf("notice covers %q, want %q", n.Path, "/a")
	}
	if n.Reason == "" {
		t.Error("notice has no reason")
	}
}

// An already-pending path stays mergeable at capacity: refusing it would drop
// changes to the very files being edited most.
func TestCapDoesNotRejectAnAlreadyPendingPath(t *testing.T) {
	c := newCoalescer(10*time.Millisecond, time.Second, 1)

	c.add(base, ev("/a.go", workspace.OpWrite))
	c.add(base.Add(time.Millisecond), ev("/a.go", workspace.OpAttrib))

	events, notices := c.flush(base.Add(time.Second))
	if len(notices) != 0 {
		t.Errorf("merging into a pending path counted as a loss: %+v", notices)
	}
	if len(events) != 1 || events[0].Op != workspace.OpWrite|workspace.OpAttrib {
		t.Errorf("got %+v, want one merged event", events)
	}
}

// Notices are cleared once reported, or every later flush would repeat a loss
// that has already been accounted for and the agent would go coarse forever.
func TestNoticesAreClearedAfterFlush(t *testing.T) {
	c := newCoalescer(10*time.Millisecond, time.Second, 0)
	c.overflow(workspace.ExportCWD, "/x", 5)

	if _, notices := c.flush(base); len(notices) != 1 {
		t.Fatalf("first flush gave %d notices, want 1", len(notices))
	}
	if _, notices := c.flush(base.Add(time.Second)); len(notices) != 0 {
		t.Errorf("second flush repeated the notice: %+v", notices)
	}
}

func TestCommonDir(t *testing.T) {
	tests := []struct{ a, b, want string }{
		{"/a/b", "/a/b", "/a/b"},
		{"/a/b", "/a/c", "/a"},
		{"/a/b/c", "/a/b/d", "/a/b"},
		{"/a", "/b", "/"},
		{"/", "/a", "/"},
		{"/a/b", "/", "/"},
		// A shared name prefix is not a shared directory: /ab is not under /a.
		{"/a", "/ab", "/"},
		{"/a/bc", "/a/bd", "/a"},
	}
	for _, tt := range tests {
		if got := commonDir(tt.a, tt.b); got != tt.want {
			t.Errorf("commonDir(%q, %q) = %q, want %q", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestNextDue(t *testing.T) {
	c := newCoalescer(40*time.Millisecond, 250*time.Millisecond, 0)

	if _, ok := c.nextDue(base); ok {
		t.Error("nextDue reported work with nothing pending")
	}

	c.add(base, ev("/a.go", workspace.OpWrite))
	at, ok := c.nextDue(base)
	if !ok {
		t.Fatal("nextDue reported nothing with an event pending")
	}
	if want := base.Add(40 * time.Millisecond); !at.Equal(want) {
		t.Errorf("nextDue = %v, want %v (debounce)", at, want)
	}

	// Once the max delay is nearer than the debounce, it governs.
	c.add(base.Add(240*time.Millisecond), ev("/a.go", workspace.OpWrite))
	at, _ = c.nextDue(base.Add(240 * time.Millisecond))
	if want := base.Add(250 * time.Millisecond); !at.Equal(want) {
		t.Errorf("nextDue = %v, want %v (max delay)", at, want)
	}
}

// Everything the coalescer emits goes on the wire, so it must satisfy the
// validation the agent applies. A path this package can produce but the agent
// refuses would present as "notifications do not work for this one file".
func TestFlushedEventsAreValid(t *testing.T) {
	c := newCoalescer(time.Millisecond, time.Second, 0)
	for _, p := range []string{"/", "/a.go", "/src/deep/nested/file.ts", "/.env", "/café.txt"} {
		c.add(base, ev(p, workspace.OpWrite))
	}
	events, _ := c.flush(base.Add(time.Second))
	if len(events) != 5 {
		t.Fatalf("flushed %d events, want 5", len(events))
	}
	for _, e := range events {
		if err := e.Validate(); err != nil {
			t.Errorf("coalescer produced %+v, which the agent would reject: %v", e, err)
		}
	}
}

// Two exports are independent: a storm under one must not be reported against
// the other, or the agent goes coarse over a tree that is perfectly fine.
func TestNoticesArePerExport(t *testing.T) {
	c := newCoalescer(time.Millisecond, time.Second, 0)
	c.overflow("/cwd", "/a", 3)
	c.overflow("/m/0123456789abcdef", "/b", 7)

	_, notices := c.flush(base.Add(time.Second))
	if len(notices) != 2 {
		t.Fatalf("got %d notices, want one per export", len(notices))
	}
	byExport := map[string]workspace.FSNotice{}
	for _, n := range notices {
		byExport[n.Export] = n
	}
	if byExport["/cwd"].Dropped != 3 || byExport["/m/0123456789abcdef"].Dropped != 7 {
		t.Errorf("losses were mixed between exports: %+v", notices)
	}
}
