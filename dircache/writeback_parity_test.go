package dircache

// A write=back share behaves like a bind mount: what the container changes
// comes back, last writer wins, and the side that lost is said (ADR 0044).
// These cases are files the cache never held, which is every existing file
// while prefetch is off.

import (
	"context"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// An existing file the container rewrote, and nobody touched here since,
// arrives. The cache never held it: with prefetch off that is every file.
func TestAnExistingFileTheContainerRewroteIsCarriedBack(t *testing.T) {
	root := t.TempDir()
	writeLocal(t, root, "index.json", "{}\n")

	store := &faultStore{}
	store.add("/index.json", "{\"from\":\"the container\"}\n")
	c, _, clk := wbCache(t, store, root)
	rounds(t, c, clk, 1)

	if got, _ := os.ReadFile(filepath.Join(root, "index.json")); string(got) != "{\"from\":\"the container\"}\n" {
		t.Errorf("index.json holds %q, want the container's version", got)
	}
}

// A file changed here after the container's change is this machine's: it is
// kept, and the container's version is named in status rather than dropped
// without a word.
func TestAnExistingFileChangedHereLaterIsKeptAndReported(t *testing.T) {
	root := t.TempDir()
	writeLocal(t, root, "out.txt", "written here last\n")

	store := &faultStore{mtime: time.Now().Add(-time.Hour).Truncate(time.Second)}
	store.add("/out.txt", "written in the container first\n")
	c, _, clk := wbCache(t, store, root)
	rounds(t, c, clk, 2)

	if got, _ := os.ReadFile(filepath.Join(root, "out.txt")); string(got) != "written here last\n" {
		t.Errorf("out.txt holds %q, want this machine's later version kept", got)
	}
	reports := c.Reports()
	if len(reports) != 1 {
		t.Fatalf("reports = %+v, want one share", reports)
	}
	var named bool
	for _, sk := range reports[0].WriteBack.Skipped {
		if sk.Path == "/out.txt" {
			named = true
		}
	}
	if !named {
		t.Errorf("status does not name the container's change that was not carried back: %+v", reports[0].WriteBack)
	}
}

// A copy is not a change. An entrypoint's chown -R copies every file into the
// cache layer with its size and time unchanged; reading that as the container
// writing would rewrite the whole tree.
func TestACopyOfAnExistingFileIsNotAChange(t *testing.T) {
	root := t.TempDir()
	writeLocal(t, root, "lib/util.go", "package lib\n")
	info, err := os.Stat(filepath.Join(root, "lib", "util.go"))
	if err != nil {
		t.Fatal(err)
	}

	store := &faultStore{content: map[string]string{"/lib/util.go": "package lib\n"}, mtime: info.ModTime()}
	store.changes = []Change{{Path: "/lib/util.go", Size: info.Size(), ModTime: info.ModTime().UnixNano()}}
	c, _, clk := wbCache(t, store, root)
	rounds(t, c, clk, 2)

	if len(store.pulled) != 0 {
		t.Errorf("a copy of an unchanged file was fetched: %v", store.pulled)
	}
	after, err := os.Stat(filepath.Join(root, "lib", "util.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(info.ModTime()) {
		t.Errorf("the file was rewritten: mtime %v, was %v", after.ModTime(), info.ModTime())
	}
}

// A cache the fill could not finish (prefetch ran out of budget) still carries
// back. What the fill never sent is decided like any other file the cache never
// held.
func TestAnIncompleteCacheStillCarriesBack(t *testing.T) {
	root := t.TempDir()
	writeLocal(t, root, "index.json", "{}\n")

	store := &faultStore{}
	store.add("/index.json", "rewritten in the container\n")
	store.add("/made-there.txt", "new in the container\n")
	c, _, clk := wbCache(t, store, root)
	c.shares.set(wbShare, root, &shareState{Report: Report{Done: true}, Cached: false}, nil)
	rounds(t, c, clk, 1)

	for rel, want := range map[string]string{
		"index.json":     "rewritten in the container\n",
		"made-there.txt": "new in the container\n",
	} {
		got, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil || string(got) != want {
			t.Errorf("%s holds %q (%v), want %q", rel, got, err, strings.TrimSpace(want))
		}
	}
}

// setTime gives a file here a time, which modTimeAsChangeTime reads as its
// change time.
func setTime(t *testing.T, root, rel string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(filepath.Join(root, filepath.FromSlash(rel)), at, at); err != nil {
		t.Fatal(err)
	}
}

// A deletion of a file the cache never held is carried back when the container
// deleted it after this machine last changed it, and kept, and said, when this
// machine changed it later.
func TestADeletionOfAFileTheCacheNeverHeldGoesByWhoWroteLast(t *testing.T) {
	root := t.TempDir()
	now := time.Now().Truncate(time.Second)
	writeLocal(t, root, "gone.txt", "deleted in the container last")
	writeLocal(t, root, "mine.txt", "changed here after the container deleted it")
	setTime(t, root, "gone.txt", now.Add(-time.Hour))
	setTime(t, root, "mine.txt", now.Add(time.Hour))

	store := &faultStore{changes: []Change{
		{Path: "/gone.txt", Deleted: true, Changed: now.UnixNano()},
		{Path: "/mine.txt", Deleted: true, Changed: now.UnixNano()},
	}}
	c, _, clk := wbCache(t, store, root)
	rounds(t, c, clk, 2)

	if exists(root, "gone.txt") {
		t.Error("a file the container deleted last is still here")
	}
	if !exists(root, "mine.txt") {
		t.Error("a file changed here after the container deleted it was deleted")
	}
	if sk := c.Reports()[0].WriteBack.Skipped; len(sk) != 1 || sk[0].Path != "/mine.txt" || sk[0].Kind != SkipKept {
		t.Errorf("status lists %+v, want mine.txt kept", sk)
	}
}

// A time that is not known never deletes or overwrites. An older workspace
// sends no change time, so its deletion is kept and named with the remedy; a
// machine whose change times cannot be read keeps every file the cache never
// held.
func TestUnknownTimesNeverWriteOrDelete(t *testing.T) {
	root := t.TempDir()
	writeLocal(t, root, "gone.txt", "an older workspace deleted it")
	writeLocal(t, root, "index.json", "{}")

	store := &faultStore{changes: []Change{{Path: "/gone.txt", Deleted: true}}}
	c, _, clk := wbCache(t, store, root)
	rounds(t, c, clk, 2)
	if !exists(root, "gone.txt") {
		t.Error("a deletion with no change time was carried back")
	}
	if sk := c.Reports()[0].WriteBack.Skipped; len(sk) != 1 || !strings.Contains(sk[0].Why, "update the workspace") {
		t.Errorf("status lists %+v, want the deletion kept with the remedy", sk)
	}

	blind := &faultStore{}
	blind.add("/index.json", "rewritten in the container")
	c2, _, clk2 := wbCache(t, blind, root)
	c2.ChangeTime = nil
	rounds(t, c2, clk2, 2)
	if got, _ := os.ReadFile(filepath.Join(root, "index.json")); string(got) != "{}" {
		t.Errorf("with no change time here, index.json was overwritten: %q", got)
	}
}

// A directory the container replaced (rm -rf dir && mkdir dir) takes with it
// what this machine had there that the cache never held, files and
// directories, once the container's own view confirms each is gone and only if
// nothing changed here after the replace. A directory it removed outright goes
// with them, emptied first.
func TestAReplacedOrRemovedDirectoryTakesFilesTheCacheNeverHeld(t *testing.T) {
	root := t.TempDir()
	now := time.Now().Truncate(time.Second)
	for _, rel := range []string{"dir/a", "dir/sub/b", "dir/edited", "dir/present", "gone/x", "gone/deep/y"} {
		writeLocal(t, root, rel, rel)
		setTime(t, root, rel, now.Add(-time.Hour))
	}
	setTime(t, root, "dir/edited", now.Add(time.Hour))
	// A directory's own change time moves with what is made in it, so these
	// were last changed before the container's removal too.
	for _, rel := range []string{"dir/sub", "gone/deep", "gone"} {
		setTime(t, root, rel, now.Add(-time.Hour))
	}

	store := &faultStore{changes: []Change{
		{Path: "/dir", Opaque: true, Changed: now.UnixNano()},
		{Path: "/gone", Deleted: true, Changed: now.UnixNano()},
	}}
	store.presentFn = func(paths []string) ([]string, error) {
		var here []string
		for _, p := range paths {
			if p == "/dir/present" {
				here = append(here, p)
			}
		}
		return here, nil
	}
	c, _, clk := wbCache(t, store, root)
	rounds(t, c, clk, 3)

	for _, rel := range []string{"dir/a", "dir/sub/b", "dir/sub", "gone/x", "gone/deep", "gone"} {
		if exists(root, rel) {
			t.Errorf("%s is still here after the container removed it", rel)
		}
	}
	for _, rel := range []string{"dir", "dir/edited", "dir/present"} {
		if !exists(root, rel) {
			t.Errorf("%s was removed: it was changed here later, the container still has it, or it is the replaced directory itself", rel)
		}
	}
}

// Without a time for the replace (an older workspace), nothing the cache never
// held is taken from under it.
func TestAReplacedDirectoryWithNoTimeTakesNothingTheCacheNeverHeld(t *testing.T) {
	root := t.TempDir()
	writeLocal(t, root, "dir/a", "a")
	store := &faultStore{changes: []Change{{Path: "/dir", Opaque: true}}}
	c, _, clk := wbCache(t, store, root)
	rounds(t, c, clk, 2)
	if !exists(root, "dir/a") {
		t.Error("a file the cache never held was deleted on a replace with no time")
	}
}

// An empty directory the container made arrives.
func TestAnEmptyDirectoryTheContainerMadeArrives(t *testing.T) {
	root := t.TempDir()
	store := &faultStore{changes: []Change{{Path: "/emptydir/inner", Dir: true, Changed: time.Now().UnixNano()}}}
	c, _, clk := wbCache(t, store, root)
	rounds(t, c, clk, 1)
	if info, err := os.Stat(filepath.Join(root, "emptydir", "inner")); err != nil || !info.IsDir() {
		t.Errorf("the directory did not arrive: %v", err)
	}
}

// chmod +x in the container on a file that exists here arrives as the execute
// bit, and nothing else about the mode: the share reports the rest wide.
func TestChmodPlusXInTheContainerArrives(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows keeps no execute bit, and the share reports every file executable")
	}
	root := t.TempDir()
	writeLocal(t, root, "script.sh", "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(root, "script.sh"), 0o640); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	setTime(t, root, "script.sh", past)

	store := &faultStore{changes: []Change{{
		Path: "/script.sh", Size: int64(len("#!/bin/sh\n")), ModTime: past.UnixNano(),
		Changed: time.Now().UnixNano(), Mode: 0o777,
	}}}
	c, _, clk := wbCache(t, store, root)
	c.shares.set(wbShare, root, &shareState{Report: Report{Done: true}, opts: ShareOptions{Exec: 0o111}}, nil)
	rounds(t, c, clk, 2)

	info, err := os.Stat(filepath.Join(root, "script.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o750 {
		t.Errorf("mode = %o, want 0750: the execute bit added where the file is readable, nothing else", info.Mode().Perm())
	}
	if len(store.pulled) != 0 {
		t.Errorf("a mode change fetched the file: %v", store.pulled)
	}
}

// A host edit made before a container write must not overwrite it. The edit
// reaches the cache 150ms after it is made, and a container writing in that
// window wrote last: invalidation leaves it to write-back, which carries the
// container's version. An edit here after the container's is sent as before.
func TestAnInvalidationDoesNotOverwriteALaterContainerWrite(t *testing.T) {
	root := t.TempDir()
	now := time.Now().Truncate(time.Second)
	writeLocal(t, root, "theirs-later.txt", "edited here first")
	writeLocal(t, root, "yours-later.txt", "edited here last")
	setTime(t, root, "theirs-later.txt", now.Add(-time.Minute))
	setTime(t, root, "yours-later.txt", now.Add(time.Minute))

	store := &faultStore{changes: []Change{
		{Path: "/theirs-later.txt", Size: 99, ModTime: now.UnixNano(), Changed: now.UnixNano()},
		{Path: "/yours-later.txt", Size: 99, ModTime: now.UnixNano(), Changed: now.UnixNano()},
	}}
	c, _, _ := wbCache(t, store, root)
	c.Observe(Event{Share: wbShare, Path: "/theirs-later.txt", Op: OpWrite})
	c.Observe(Event{Share: wbShare, Path: "/yours-later.txt", Op: OpWrite})
	c.flushNow(t.Context())

	var sent []string
	for _, e := range store.applied {
		sent = append(sent, e.Path)
	}
	if strings.Join(sent, ",") != "yours-later.txt" {
		t.Errorf("invalidation sent %v, want only the file this machine wrote last", sent)
	}
}

// Settle carries back what is pending without waiting for the next tick, round
// after round until a round carries nothing, and sends what is waiting to be
// invalidated first.
func TestSettleCarriesEverythingBeforeTheSessionCloses(t *testing.T) {
	root := t.TempDir()
	store := &faultStore{}
	store.add("/first.txt", "made just before the close")
	calls := 0
	store.changesFn = func() error {
		calls++
		if calls == 2 {
			// Made while the first round ran.
			store.add("/second.txt", "made during the first round")
		}
		return nil
	}
	c, _, _ := wbCache(t, store, root)
	writeLocal(t, root, "edited.txt", "edited here")
	c.Observe(Event{Share: wbShare, Path: "/edited.txt", Op: OpWrite})

	c.Settle(t.Context())

	for rel, want := range map[string]string{
		"first.txt":  "made just before the close",
		"second.txt": "made during the first round",
	} {
		if got, _ := os.ReadFile(filepath.Join(root, rel)); string(got) != want {
			t.Errorf("%s holds %q after Settle, want %q", rel, got, want)
		}
	}
	if len(store.applied) != 1 || store.applied[0].Path != "edited.txt" {
		t.Errorf("Settle sent %+v, want the pending edit", store.applied)
	}
}

// Settle is bounded by its context, and says what it left.
func TestSettleStopsAtItsBoundAndSaysWhatIsLeft(t *testing.T) {
	root := t.TempDir()
	store := &faultStore{}
	store.add("/stuck.txt", "never fetched")
	store.pullFn = func(ctx context.Context, _ []string, _ func(string) error) error {
		<-ctx.Done()
		return ctx.Err()
	}
	c, logs, _ := wbCache(t, store, root)
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		c.Settle(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Settle outlived its context")
	}
	if !strings.Contains(logs.String(), "did not finish before the session closed") || !strings.Contains(logs.String(), "stuck.txt") {
		t.Errorf("Settle left a change without saying so:\n%s", logs.String())
	}
}

// The property the rules rest on: whatever the mix of what the cache holds,
// what changed where and when, no decision writes, deletes or changes the mode
// of a file whose content or existence changed here AFTER the container's
// change.
func TestNothingChangedHereLaterIsEverOverwritten(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	base := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	for i := range 5000 {
		const p = "/f"
		at := func() time.Time { return base.Add(time.Duration(rng.IntN(5)) * time.Minute) }
		manifest := map[string]Baseline{}
		held := rng.IntN(2) == 0
		baseline := Baseline{Size: int64(rng.IntN(3)), ModTime: at()}
		if held {
			manifest[p] = baseline
		}
		here := rng.IntN(4) != 0
		localSize, localMod, localCtime := int64(rng.IntN(3)), at(), at()
		ch := Change{Path: p, Changed: at().UnixNano(), Mode: fs.FileMode(rng.IntN(2) * 0o111)}
		switch rng.IntN(3) {
		case 0:
			ch.Deleted = true
		default:
			ch.Size, ch.ModTime = int64(rng.IntN(3)), at().UnixNano()
		}
		skew := time.Duration(rng.IntN(3)-1) * time.Minute
		local := func(string) (fs.FileInfo, bool) {
			return modeStub{stub{size: localSize, mod: localMod}, fs.FileMode(rng.IntN(2) * 0o111)}, here
		}
		ctime := func(string) (time.Time, bool) { return localCtime, true }

		actions := decide(manifest, nil, []Change{ch}, local, ctime, skew, 0o111)

		laterHere := here && localCtime.After(time.Unix(0, ch.Changed).Add(-skew))
		changedHere := !held || localSize != baseline.Size || !localMod.Equal(baseline.ModTime)
		if !laterHere || !changedHere {
			continue
		}
		for _, a := range actions {
			if a.kind == kindWrite || a.kind == kindDelete || a.kind == kindChmod || (a.kind == kindConflict && a.Wins) {
				t.Fatalf("case %d: held=%v here=%v change=%+v local size=%d mod=%v ctime=%v skew=%v: decided %+v, which takes a later change here",
					i, held, here, ch, localSize, localMod, localCtime, skew, a)
			}
		}
	}
}

// A file deleted here while the client ran leaves a whiteout in the cache (the
// deletion is dropped through the union). Made here again while nothing ran,
// the fill must still send it: the whiteout is older than the file, and kept
// out, the file would stay hidden from the container for good. A deletion the
// container made after the file here changed is still never sent over.
func TestAFillSendsAFileMadeAgainOverItsOwnOlderDeletion(t *testing.T) {
	root := t.TempDir()
	now := time.Now().Truncate(time.Second)
	writeLocal(t, root, "made-again.txt", "made here again")
	writeLocal(t, root, "deleted-there.txt", "older than the container's deletion")
	setTime(t, root, "made-again.txt", now.Add(time.Minute))
	setTime(t, root, "deleted-there.txt", now.Add(-time.Minute))

	store := &faultStore{changes: []Change{
		{Path: "/made-again.txt", Deleted: true, Changed: now.UnixNano()},
		{Path: "/deleted-there.txt", Deleted: true, Changed: now.UnixNano()},
	}}
	c := &Cache{
		Store: func() (Store, bool) { return store, true }, Ctx: t.Context(),
		ChangeTime: modTimeAsChangeTime,
	}
	t.Cleanup(c.Stop)
	c.Attach(wbShare, root, ShareOptions{Policy: PolicyEager})
	eventually(t, "the fill to finish", func() bool {
		r := c.Reports()
		return len(r) == 1 && r[0].Done
	})

	store.mu.Lock()
	var sent []string
	for _, e := range store.applied {
		sent = append(sent, e.Path)
	}
	store.mu.Unlock()
	if strings.Join(sent, ",") != "made-again.txt" {
		t.Errorf("the fill sent %v, want only the file made here after its deletion", sent)
	}
}
