package dircache

// What write-back does about a removed or replaced directory, a link, a name
// this machine cannot hold, and a stall (ADR 0044).

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// A directory the container replaced: what the fill sent under it goes, a file
// of the same name the container made is a write, a file changed here since it
// was sent is kept and reported, and what the fill never sent is left alone.
func TestAReplacedDirectoryDeletesWhatTheFillSentUnderIt(t *testing.T) {
	manifest := map[string]Baseline{
		"/tools/old":      {Size: 10, ModTime: sentAt},
		"/tools/same":     {Size: 10, ModTime: sentAt},
		"/tools/edited":   {Size: 10, ModTime: sentAt},
		"/tools/sub/deep": {Size: 10, ModTime: sentAt},
		"/other/kept":     {Size: 10, ModTime: sentAt},
	}
	local := localFrom(map[string]stub{
		"/tools/old":         {size: 10, mod: sentAt},
		"/tools/same":        {size: 10, mod: sentAt},
		"/tools/edited":      {size: 99, mod: laterHere},
		"/tools/sub/deep":    {size: 10, mod: sentAt},
		"/tools/never-sent":  {size: 5, mod: sentAt},
		"/other/kept":        {size: 10, mod: sentAt},
		"/tools/not-in-fill": {size: 5, mod: sentAt},
	})
	changes := []Change{
		{Path: "/tools", Opaque: true},
		{Path: "/tools/same", Size: 20, ModTime: laterHere.UnixNano()},
		{Path: "/tools/new", Size: 3, ModTime: laterHere.UnixNano()},
	}

	actions := decide(manifest, nil, expandReplaced(manifest, changes), local, nil, 0, 0, 0)

	want := map[string]kind{
		"/tools/old":      kindDelete,
		"/tools/sub/deep": kindDelete,
		"/tools/edited":   kindConflict,
		"/tools/same":     kindWrite,
		"/tools/new":      kindWrite,
	}
	got := map[string]kind{}
	for _, a := range actions {
		got[a.Path] = a.kind
	}
	for p, k := range want {
		if got[p] != k {
			t.Errorf("%s: %v, want %v; actions = %+v", p, got[p], k, actions)
		}
	}
	for _, p := range []string{"/other/kept", "/tools/never-sent", "/tools/not-in-fill"} {
		if _, ok := got[p]; ok {
			t.Errorf("%s was touched although it is outside what the fill sent under the directory", p)
		}
	}
}

// A directory removed outright is one deletion for the directory, and the files
// the fill sent under it are deleted with it.
func TestARemovedDirectoryDeletesWhatTheFillSentUnderIt(t *testing.T) {
	manifest := map[string]Baseline{
		"/gone/a":    {Size: 1, ModTime: sentAt},
		"/gone/b/c":  {Size: 1, ModTime: sentAt},
		"/gone-not":  {Size: 1, ModTime: sentAt},
		"/gone.file": {Size: 1, ModTime: sentAt},
	}
	local := localFrom(map[string]stub{
		"/gone/a": {size: 1, mod: sentAt}, "/gone/b/c": {size: 1, mod: sentAt},
		"/gone-not": {size: 1, mod: sentAt}, "/gone.file": {size: 1, mod: sentAt},
	})
	changes := []Change{{Path: "/gone", Deleted: true}}

	actions := decide(manifest, nil, expandReplaced(manifest, changes), local, nil, 0, 0, 0)

	var got []string
	for _, a := range actions {
		if a.kind == kindDelete {
			got = append(got, a.Path)
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, []string{"/gone/a", "/gone/b/c"}) {
		t.Errorf("deleted %v, want exactly what lay under /gone (not /gone-not or /gone.file)", got)
	}
}

// The agent reads a link out of the layer, and what to do about it depends on
// what this machine has there.
func TestDecideLinks(t *testing.T) {
	mod := sentAt
	changes := func(p, target string) []Change { return []Change{{Path: p, Link: target}} }
	kinds := func(as []action) []kind {
		var out []kind
		for _, a := range as {
			out = append(out, a.kind)
		}
		return out
	}

	nothing := func(string) linkState { return linkState{} }
	if got := kinds(decideLinks(nil, changes("/bin/tool", "../lib/tool"), nothing)); !slices.Equal(got, []kind{kindLink}) {
		t.Errorf("a link where nothing is: %v, want it made", got)
	}

	same := func(string) linkState {
		return linkState{Exists: true, IsLink: true, Target: "../lib/tool", ModTime: mod}
	}
	if got := decideLinks(nil, changes("/bin/tool", "../lib/tool"), same); len(got) != 0 {
		t.Errorf("a link already pointing there produced %+v", got)
	}

	elsewhere := func(string) linkState {
		return linkState{Exists: true, IsLink: true, Target: "/usr/bin/tool", ModTime: mod}
	}
	got := decideLinks(nil, changes("/bin/tool", "../lib/tool"), elsewhere)
	if len(got) != 1 || got[0].kind != kindConflict || got[0].Wins {
		t.Errorf("a link here pointing elsewhere: %+v, want a conflict that keeps yours", got)
	}

	// One this write-back made and nobody has touched since: the container
	// re-pointed it, and that is an edit to carry.
	ours := map[string]Baseline{"/bin/tool": linkBaseline("/usr/bin/tool", mod)}
	if got := kinds(decideLinks(ours, changes("/bin/tool", "../lib/tool"), elsewhere)); !slices.Equal(got, []kind{kindLink}) {
		t.Errorf("a link this write-back made, re-pointed in the container: %v, want it replaced", got)
	}

	file := func(string) linkState { return linkState{Exists: true} }
	got = decideLinks(nil, changes("/bin/tool", "../lib/tool"), file)
	if len(got) != 1 || got[0].kind != kindConflict || got[0].Wins {
		t.Errorf("a link where a file is: %+v, want a conflict that keeps the file", got)
	}
}

// A link is made as it was written, however absolute or far outside the share:
// making it writes nothing through it. What a later write does with it is
// os.Root's to refuse.
func TestLandLinkKeepsTheTargetVerbatim(t *testing.T) {
	root := t.TempDir()
	r := openRoot(t, root)
	outside := t.TempDir()

	for _, target := range []string{"../elsewhere/tool", outside, "relative/inside"} {
		if err := landLink(r, "bin/tool", target); err != nil {
			if skipUnprivileged(t, err) {
				return
			}
			t.Fatalf("landLink(%q): %v", target, err)
		}
		got, err := os.Readlink(filepath.Join(root, "bin", "tool"))
		if err != nil || got != target {
			t.Errorf("link target = %q (%v), want %q verbatim", got, err, target)
		}
	}
	entries, _ := os.ReadDir(filepath.Join(root, "bin"))
	if len(entries) != 1 {
		t.Errorf("bin holds %v, want only the link and no temporary", entries)
	}

	// And a write through the link it made still goes nowhere.
	err := writeUnder(r, File{Path: "/bin/tool/escaped", Mode: 0o644, Body: strings.NewReader("x")}, nil)
	if err == nil {
		t.Error("a write through a link out of the share succeeded")
	}
	if _, err := os.Stat(filepath.Join(outside, "escaped")); err == nil {
		t.Error("a write went through the link and out of the share")
	}
}

// skipUnprivileged skips a test on an account that may not make a link.
func skipUnprivileged(t *testing.T, err error) bool {
	t.Helper()
	if classifyWrite(err) == faultName && runtime.GOOS == "windows" {
		t.Skipf("symlink creation is privileged on this account: %v", err)
		return true
	}
	return false
}

// A round carries a replaced directory back whole: the new file, the old one
// gone, and a link made. The files landed move the manifest and the record.
func TestWriteBackCarriesAReplacedDirectoryAndALink(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{"tools/old", "tools/sub/deep"} {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	store := &faultStore{}
	store.add("/tools/new", "fresh")
	store.changes = append(store.changes,
		Change{Path: "/tools", Opaque: true},
		Change{Path: "/bin/tool", Link: "../tools/new"})

	c, _, clk := wbCache(t, store, root)
	c.shares.noteSent(wbShare, root, []Entry{{Path: "tools/old", Size: 3}, {Path: "tools/sub/deep", Size: 3}})
	c.makeLink = func(r *os.Root, rel, target string) error { return r.Mkdir("linked", 0o755) }
	rounds(t, c, clk, 1)

	for _, gone := range []string{"tools/old", "tools/sub/deep"} {
		if exists(root, gone) {
			t.Errorf("%s is still here after the container replaced its directory", gone)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(root, "tools", "new")); string(got) != "fresh" {
		t.Errorf("tools/new holds %q, want the container's file", got)
	}
	if !exists(root, "linked") {
		t.Error("the link was not made")
	}
	if _, ok := c.shares.baselines(wbShare)["/tools/old"]; ok {
		t.Error("a deleted file is still in the manifest")
	}
}

// A link this account may not create is given up on by path, counted as a link
// and named in status, and nothing stands in for it.
func TestALinkThisAccountMayNotMakeIsSkippedAndNamed(t *testing.T) {
	root := t.TempDir()
	store := &faultStore{}
	store.add("/after", "x")
	store.changes = append(store.changes, Change{Path: "/bin/tool", Link: "../x"})

	c, logs, clk := wbCache(t, store, root)
	attempts := 0
	c.makeLink = func(*os.Root, string, string) error {
		attempts++
		return &os.LinkError{Op: "symlink", Old: "../x", New: "bin/tool", Err: errNoSymlink}
	}
	rounds(t, c, clk, 4)

	if attempts != 1 {
		t.Errorf("the link was tried %d times, want once and then left", attempts)
	}
	if !exists(root, "after") {
		t.Error("a file behind the link that cannot be made never arrived")
	}
	if exists(root, "bin/tool") {
		t.Error("something stands in for the link")
	}
	skipped := c.Reports()[0].WriteBack.Skipped
	if len(skipped) != 1 || skipped[0].Path != "/bin/tool" || skipped[0].Kind != SkipLink {
		t.Errorf("status names %+v, want the one link", skipped)
	}
	if n := strings.Count(logs.String(), "bin/tool"); n < 1 {
		t.Errorf("the link is not named in the log:\n%s", logs.String())
	}
}

// A file changed again after it was given up on is tried again: the skip is for
// that change and not for the path for ever.
func TestASkippedFileIsTriedAgainWhenItChanges(t *testing.T) {
	root := t.TempDir()
	store := &faultStore{}
	store.add("/odd", "first")
	c, _, clk := wbCache(t, store, root)
	refuse := true
	attempts := 0
	c.writeFile = func(r *os.Root, f File) error {
		attempts++
		if refuse {
			return &os.PathError{Op: "openat", Path: f.Path, Err: errBadName}
		}
		return writeUnder(r, f, nil)
	}
	rounds(t, c, clk, 3)
	if attempts != 1 {
		t.Fatalf("tried %d times, want once", attempts)
	}

	refuse = false
	store.mu.Lock()
	store.changes = []Change{{Path: "/odd", Size: 99, ModTime: store.mtime.Add(time.Minute).UnixNano()}}
	store.content["/odd"] = "second, and different"
	store.mu.Unlock()
	rounds(t, c, clk, 1)

	if got, _ := os.ReadFile(filepath.Join(root, "odd")); string(got) != "second, and different" {
		t.Errorf("odd holds %q, want the changed version", got)
	}
	if len(c.Reports()[0].WriteBack.Skipped) != 0 {
		t.Errorf("the path is still listed as given up on: %+v", c.Reports()[0].WriteBack.Skipped)
	}
}

// A stall is said once, backed off from, and ended with one line. A test cannot
// wait out a backoff, so the clock is the thing that moves.
func TestAStallIsSaidOnceBackedOffFromAndEnded(t *testing.T) {
	root := t.TempDir()
	store := &faultStore{}
	store.add("/f", "x")
	failing := true
	asked := 0
	store.changesFn = func() error {
		asked++
		if failing {
			return errors.New("the channel reset")
		}
		return nil
	}
	c, logs, clk := wbCache(t, store, root)

	c.writeBackRound(t.Context())
	c.writeBackRound(t.Context())
	if asked != 1 {
		t.Errorf("the store was asked %d times with no time passed, want the backoff to hold the second", asked)
	}
	clk.advance(writeBackBackoffMin)
	c.writeBackRound(t.Context())
	clk.advance(writeBackBackoffMin)
	c.writeBackRound(t.Context())
	if asked != 2 {
		t.Errorf("asked %d times, want the wait to have doubled (%d asks)", asked, 2)
	}

	wb := c.Reports()[0].WriteBack
	if !wb.Stalled || !strings.Contains(wb.Reason, "the channel reset") {
		t.Errorf("status = %+v, want it stalled with the reason", wb)
	}
	if n := strings.Count(logs.String(), "write-back is stalled"); n != 1 {
		t.Errorf("the stall was said %d times, want once:\n%s", n, logs.String())
	}

	failing = false
	clk.advance(2 * writeBackBackoffMax)
	c.writeBackRound(t.Context())
	if !exists(root, "f") {
		t.Error("the file did not land once the channel came back")
	}
	if c.Reports()[0].WriteBack.Stalled || !strings.Contains(logs.String(), "write-back is moving again") {
		t.Errorf("the recovery was not said and cleared:\n%s", logs.String())
	}
}

// The wait doubles from its floor to its ceiling and no further.
func TestTheBackoffDoublesToItsCeiling(t *testing.T) {
	var c Cache
	c.shares.set(wbShare, t.TempDir(), &shareState{Report: Report{Done: true}}, nil)
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

	var waits []time.Duration
	for range 8 {
		_, next := c.shares.wb.fail(wbShare, now, "x", "", 0, false)
		waits = append(waits, next.Sub(now))
	}
	want := []time.Duration{5, 10, 20, 40, 60, 60, 60, 60}
	for i, w := range want {
		if waits[i] != w*time.Second {
			t.Errorf("wait %d = %v, want %v (all: %v)", i, waits[i], w*time.Second, waits)
		}
	}

	c.shares.wb.ok(wbShare)
	if _, next := c.shares.wb.fail(wbShare, now, "x", "", 0, false); next.Sub(now) != writeBackBackoffMin {
		t.Errorf("after a success the wait is %v, want it reset to %v", next.Sub(now), writeBackBackoffMin)
	}

	// A round that landed something and still failed waits only the floor, so
	// one file that never lands cannot slow every other to once a minute.
	for range 5 {
		c.shares.wb.fail(wbShare, now, "x", "", 0, false)
	}
	if _, next := c.shares.wb.fail(wbShare, now, "x", "", 0, true); next.Sub(now) != writeBackBackoffMin {
		t.Errorf("after a round with progress the wait is %v, want %v", next.Sub(now), writeBackBackoffMin)
	}
}

// An agent from before the markers were read reports fuse-overlayfs's files as
// the container's. Written into the tree they are files nobody made, so they
// are named once and never written.
func TestAnOverlayMarkerFromAnOlderAgentIsNeverWritten(t *testing.T) {
	root := t.TempDir()
	store := &faultStore{}
	store.add("/tools/.wh..wh..opq", "")
	store.add("/tools/.wh.gone", "")
	store.add("/tools/real", "kept")
	c, logs, clk := wbCache(t, store, root)

	rounds(t, c, clk, 3)

	if exists(root, "tools/.wh..wh..opq") || exists(root, "tools/.wh.gone") {
		t.Error("an overlay marker was written into the tree")
	}
	if !exists(root, "tools/real") {
		t.Error("the real file beside the markers did not arrive")
	}
	if n := strings.Count(logs.String(), "overlay marker"); n != 2 {
		t.Errorf("the markers were named %d times, want once each:\n%s", n, logs.String())
	}
}

// A name that differs only in case from one already here, on a filesystem that
// treats the two as one, would replace the other file with different contents.
// Where the filesystem tells them apart, both land.
func TestACaseCollisionIsNeverLandedOverTheOtherFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "LICENSE"), []byte("upper"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, statErr := os.Stat(filepath.Join(root, "license"))
	insensitive := statErr == nil

	store := &faultStore{}
	store.add("/license", "lower")
	c, _, clk := wbCache(t, store, root)
	rounds(t, c, clk, 2)

	got, _ := os.ReadFile(filepath.Join(root, "LICENSE"))
	if insensitive {
		if string(got) != "upper" {
			t.Errorf("LICENSE holds %q, want it untouched by a file that differs only in case", got)
		}
		sk := c.Reports()[0].WriteBack.Skipped
		if len(sk) != 1 || sk[0].Kind != SkipClash {
			t.Errorf("status names %+v, want the one collision", sk)
		}
		return
	}
	if string(got) != "upper" {
		t.Errorf("LICENSE holds %q on a case-sensitive filesystem, want it untouched", got)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "license")); string(b) != "lower" {
		t.Errorf("license holds %q, want the container's file beside LICENSE", b)
	}
}

// What the far end could not write into the cache is neither recorded nor
// compared against, and the rest of the batch is.
func TestAFillSkipsWhatTheFarEndCouldNotWrite(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"a.go", "b.go", "c.go"} {
		if err := os.WriteFile(filepath.Join(root, n), []byte("package x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rec := &fakeRecord{}
	store := &faultStore{skipPaths: map[string]bool{"b.go": true}}
	c := &Cache{Store: func() (Store, bool) { return store, true }, Record: rec, Ctx: t.Context()}
	t.Cleanup(c.Stop)

	c.Attach(wbShare, root, ShareOptions{Policy: PolicyEager})
	eventually(t, "the fill to finish", func() bool {
		r := c.Reports()
		return len(r) == 1 && r[0].Done && r[0].Err == nil
	})

	filled, _ := rec.Filled(wbShare)
	if _, ok := filled["/b.go"]; ok {
		t.Error("a file the far end left out is recorded as being in the cache")
	}
	for _, p := range []string{"/a.go", "/c.go"} {
		if b, ok := filled[p]; !ok || b.ModTime.IsZero() {
			t.Errorf("%s is recorded as %+v, want it with a baseline", p, b)
		}
	}
	if r := c.Reports()[0]; r.Sent != 2 {
		t.Errorf("%d files reported sent, want the 2 that landed", r.Sent)
	}
}

// The union cannot hold a name starting ".wh.", so the fill leaves it out and
// says so rather than failing the batch it was in.
func TestTheFillLeavesOutAnOverlayMarkerName(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"a.go", ".wh..wh..opq", ".wh.victim"} {
		if err := os.WriteFile(filepath.Join(root, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	store := &faultStore{}
	c := &Cache{Store: func() (Store, bool) { return store, true }, Ctx: t.Context()}
	t.Cleanup(c.Stop)
	c.Attach(wbShare, root, ShareOptions{Policy: PolicyEager})
	eventually(t, "the fill to finish", func() bool {
		r := c.Reports()
		return len(r) == 1 && r[0].Done && r[0].Stats.Complete()
	})

	store.mu.Lock()
	defer store.mu.Unlock()
	var sent []string
	for _, e := range store.applied {
		sent = append(sent, e.Path)
	}
	if !slices.Equal(sent, []string{"a.go"}) {
		t.Errorf("sent %v, want only a.go", sent)
	}
	if r := c.Reports()[0]; r.Stats.Markers != 2 {
		t.Errorf("the walk counted %d marker names, want 2", r.Stats.Markers)
	}
}

// expandReplaced is the changes with every candidate deletion a replaced
// directory implies, as if the consumer confirmed each absent.
func expandReplaced(manifest map[string]Baseline, changes []Change) []Change {
	for _, p := range replacedDeletions(manifest, changes) {
		changes = append(changes, Change{Path: p, Deleted: true})
	}
	return changes
}
