package dircache

// Cases where write-back could take the user's own work: a deletion implied by a
// directory the consumer replaced, a change decided minutes ago, a file the fill
// is about to send over a version not yet carried back (ADR 0044).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Present makes faultStore a Presenter. By default nothing is present in the
// consumer's view, so a deletion is confirmed.
func (f *faultStore) Present(_ context.Context, _ string, paths []string) ([]string, error) {
	f.mu.Lock()
	f.asked = append(f.asked, paths...)
	f.mu.Unlock()
	if f.presentFn != nil {
		return f.presentFn(paths)
	}
	return nil, nil
}

// noPresenter is a Store that cannot say what exists, as an agent without Carry.
type noPresenter struct{ Store }

func writeLocal(t *testing.T, root, rel, body string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The agent reports a replaced directory on every Changes while its marker
// exists, and what the client itself wrote into it (the watcher's invalidation,
// the fill) is hidden from the listing and has a baseline equal to the local
// file. Read as the consumer deleting it, that deleted the user's new and edited
// files. A deletion under a hidden directory is acted on only if the path is
// absent from the consumer's merged view, which is asked, never assumed.
func TestAFileTheClientWroteIntoAReplacedDirectoryIsNeverDeleted(t *testing.T) {
	root := t.TempDir()
	writeLocal(t, root, "D/kept", "edited by the user, then invalidated into the cache")
	writeLocal(t, root, "D/new", "created by the user")
	writeLocal(t, root, "D/old", "sent by the fill, deleted in the container")

	store := &faultStore{}
	store.changes = []Change{{Path: "/D", Opaque: true}}
	store.presentFn = func(paths []string) ([]string, error) {
		var here []string
		for _, p := range paths {
			if p != "/D/old" {
				here = append(here, p)
			}
		}
		return here, nil
	}
	c, _, clk := wbCache(t, store, root)
	c.shares.noteSent(wbShare, root, []Entry{{Path: "D/kept"}, {Path: "D/new"}, {Path: "D/old"}})

	rounds(t, c, clk, 4)

	// A restart: a new process, which knows only the record.
	c, _, clk = wbCache(t, store, root)
	c.shares.noteSent(wbShare, root, []Entry{{Path: "D/kept"}, {Path: "D/new"}})
	rounds(t, c, clk, 3)

	for _, keep := range []string{"D/kept", "D/new"} {
		if !exists(root, keep) {
			t.Errorf("%s, which the consumer still has, was deleted here", keep)
		}
	}
	if exists(root, "D/old") {
		t.Error("a file absent from the consumer's view under a replaced directory was not deleted")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, asked := range store.asked {
		if asked == "/D/kept" || asked == "/D/new" {
			return
		}
	}
	t.Errorf("the consumer was never asked about the files under D: %v", store.asked)
}

// A workspace that cannot say what exists synthesises no deletion at all.
func TestNothingIsDeletedUnderADirectoryWhenThePresenceCannotBeAsked(t *testing.T) {
	root := t.TempDir()
	writeLocal(t, root, "D/old", "x")
	store := &faultStore{changes: []Change{{Path: "/D", Opaque: true}}}
	c, _, clk := wbCache(t, noPresenter{store}, root)
	c.shares.noteSent(wbShare, root, []Entry{{Path: "D/old"}})

	rounds(t, c, clk, 2)
	if !exists(root, "D/old") {
		t.Error("a file was deleted on a directory's say-so with no way to confirm it")
	}

	failing := &faultStore{changes: []Change{{Path: "/D", Opaque: true}}, presentFn: func([]string) ([]string, error) {
		return nil, errors.New("the channel reset")
	}}
	c, _, clk = wbCache(t, failing, root)
	c.shares.noteSent(wbShare, root, []Entry{{Path: "D/old"}})
	rounds(t, c, clk, 2)
	if !exists(root, "D/old") {
		t.Error("a file was deleted although asking whether it exists failed")
	}
}

// A change set aside (a link this account cannot make) is still listed: the next
// round must not read its absence from the changes as a deletion of the file the
// user has at that path.
func TestASkippedChangeIsNotReadAsADeletionNextRound(t *testing.T) {
	root := t.TempDir()
	writeLocal(t, root, "D/F", "the user's file")
	store := &faultStore{}
	store.changes = []Change{{Path: "/D", Opaque: true}, {Path: "/D/F", Link: "../elsewhere"}}
	c, _, clk := wbCache(t, store, root)
	c.shares.noteSent(wbShare, root, []Entry{{Path: "D/F"}})
	c.makeLink = func(*os.Root, string, string) error {
		return &os.LinkError{Op: "symlink", Err: errNoSymlink}
	}

	rounds(t, c, clk, 4)

	if got, _ := os.ReadFile(filepath.Join(root, "D", "F")); string(got) != "the user's file" {
		t.Errorf("D/F holds %q after the link was skipped, want the user's file", got)
	}
}

// "Your file is kept" is said once per path, not every round.
func TestAConflictIsSaidOncePerPath(t *testing.T) {
	root := t.TempDir()
	writeLocal(t, root, "f", "edited here")
	store := &faultStore{changes: []Change{{Path: "/f", Deleted: true}}}
	c, logs, clk := wbCache(t, store, root)
	// Sent with a size and time this file no longer has: changed here since.
	c.shares.noteSent(wbShare, root, []Entry{{Path: "f"}})
	c.shares.manifests[wbShare]["/f"] = Baseline{Size: 1, ModTime: time.Unix(1, 0)}

	rounds(t, c, clk, 4)

	if n := strings.Count(logs.String(), "a file changed in both places"); n != 1 {
		t.Errorf("the conflict was said %d times in 4 rounds, want once:\n%s", n, logs.String())
	}
}

// A pass can run for minutes after decide looked at a path. A file edited here
// in between is not overwritten, and nothing that appeared here is replaced.
func TestAFileChangedHereWhileThePassRanIsNotOverwritten(t *testing.T) {
	root := t.TempDir()
	writeLocal(t, root, "a.txt", "before")
	store := &faultStore{}
	store.add("/a.txt", "the container's version")
	store.add("/n.txt", "the container's new file")
	c, _, clk := wbCache(t, store, root)
	c.shares.noteSent(wbShare, root, []Entry{{Path: "a.txt"}})

	store.pullFn = func(_ context.Context, paths []string, deliver func(string) error) error {
		// The user edits one file and creates the other while the pull runs.
		writeLocal(t, root, "a.txt", "the user's edit, made meanwhile")
		later := time.Now().Add(time.Hour)
		_ = os.Chtimes(filepath.Join(root, "a.txt"), later, later)
		writeLocal(t, root, "n.txt", "the user's own new file")
		for _, p := range paths {
			if err := deliver(p); err != nil {
				return err
			}
		}
		return nil
	}
	rounds(t, c, clk, 1)

	if got, _ := os.ReadFile(filepath.Join(root, "a.txt")); string(got) != "the user's edit, made meanwhile" {
		t.Errorf("a.txt holds %q, want the edit made while the pass ran", got)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "n.txt")); string(got) != "the user's own new file" {
		t.Errorf("n.txt holds %q, want the file that appeared here meanwhile", got)
	}
}

// One chunk that fails must not end the pass: the chunks after it are tried.
func TestAFailedChunkDoesNotStarveTheOnesAfterIt(t *testing.T) {
	root := t.TempDir()
	store := &faultStore{}
	n := 3 * pullChunk
	for i := range n {
		store.add(fmt.Sprintf("/f%04d", i), "x")
	}
	first := true
	store.pullFn = func(_ context.Context, paths []string, deliver func(string) error) error {
		if first {
			first = false
			return errors.New("the agent could not read one file of this chunk")
		}
		for _, p := range paths {
			if err := deliver(p); err != nil {
				return err
			}
		}
		return nil
	}
	c, _, clk := wbCache(t, store, root)
	rounds(t, c, clk, 1)

	landed := 0
	for i := pullChunk; i < n; i++ {
		if exists(root, fmt.Sprintf("f%04d", i)) {
			landed++
		}
	}
	if landed != n-pullChunk {
		t.Errorf("%d of the %d files after the failed chunk landed in one round", landed, n-pullChunk)
	}
}

// More than a pass's worth of files that fail to land for good must not starve
// the ones behind them.
func TestFilesThatKeepFailingDoNotStarveTheRest(t *testing.T) {
	root := t.TempDir()
	store := &faultStore{}
	total := writeBackRoundFiles + 100
	for i := range total {
		store.add(fmt.Sprintf("/f%05d", i), "x")
	}
	c, _, clk := wbCache(t, store, root)
	c.writeFile = func(r *os.Root, f File) error {
		if f.Path < fmt.Sprintf("/f%05d", writeBackRoundFiles) {
			return errors.New("held open by another program")
		}
		return writeUnder(r, f)
	}

	rounds(t, c, clk, 4)

	last := fmt.Sprintf("f%05d", total-1)
	if !exists(root, last) {
		t.Errorf("%s never landed: %d files that always fail took every slot", last, writeBackRoundFiles)
	}
}

// After a restart the fill must not send a file the consumer has changed and not
// yet carried back: it would overwrite the consumer's version in the cache and
// reset the baseline, so the both-changed rule would never run.
func TestAResumedFillDoesNotSendAPathTheConsumerChanged(t *testing.T) {
	root := t.TempDir()
	writeLocal(t, root, "main.go", "edited here since the restart")
	writeLocal(t, root, "other.go", "package x\n")
	rec := &memRecord{m: map[string]map[string]Baseline{wbShare: {
		"/main.go":  {Size: 3, ModTime: time.Unix(5, 0)}, // as sent, before the edit
		"/other.go": {Size: 3, ModTime: time.Unix(5, 0)},
	}}}
	store := &faultStore{}
	store.changes = []Change{{Path: "/main.go", Size: 9, ModTime: time.Now().UnixNano()}}
	logs := &syncBuffer{}
	clk := &fakeClock{t: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
	c := &Cache{
		Store: func() (Store, bool) { return store, true }, Record: rec, Ctx: t.Context(),
		Log: slog.New(slog.NewTextHandler(logs, nil)), now: clk.Now,
	}
	t.Cleanup(c.Stop)

	c.Attach(wbShare, root, ShareOptions{Policy: PolicyEager})
	eventually(t, "the resumed fill to finish", func() bool {
		r := c.Reports()
		return len(r) == 1 && r[0].Done
	})

	store.mu.Lock()
	sent := []string{}
	for _, e := range store.applied {
		sent = append(sent, e.Path)
	}
	store.mu.Unlock()
	if slices.Contains(sent, "main.go") {
		t.Errorf("the fill sent main.go over the consumer's version; sent %v", sent)
	}
	if !slices.Contains(sent, "other.go") {
		t.Errorf("the fill left out other.go, which the consumer did not change; sent %v", sent)
	}
	// The baseline from before the restart is what lets write-back see that
	// both sides changed it.
	if got := c.shares.baselines(wbShare)["/main.go"]; got.Size != 3 || !got.ModTime.Equal(time.Unix(5, 0)) {
		t.Errorf("main.go's baseline is %+v after the fill, want the one from before the restart", got)
	}

	rounds(t, c, clk, 2)
	if got, _ := os.ReadFile(filepath.Join(root, "main.go")); string(got) != "edited here since the restart" {
		t.Errorf("main.go holds %q; the user's later edit should win a conflict", got)
	}
	if n := strings.Count(logs.String(), "a file changed in both places"); n != 1 {
		t.Errorf("the conflict was said %d times, want once:\n%s", n, logs.String())
	}
}

// After the agent restarts it no longer knows which files the client wrote, and
// lists the fill's own copies as changes. A file changed here while nothing ran
// is still sent when the listed version is just what the fill sent before.
func TestAResumedFillSendsAFileWhoseListedVersionIsTheFillsOwn(t *testing.T) {
	root := t.TempDir()
	writeLocal(t, root, "main.go", "edited here since the restart")
	rec := &memRecord{m: map[string]map[string]Baseline{wbShare: {
		"/main.go": {Size: 3, ModTime: time.Unix(5, 0)},
	}}}
	store := &faultStore{}
	store.changes = []Change{{Path: "/main.go", Size: 3, ModTime: time.Unix(5, 0).UnixNano()}}
	c := &Cache{Store: func() (Store, bool) { return store, true }, Record: rec, Ctx: t.Context()}
	t.Cleanup(c.Stop)

	c.Attach(wbShare, root, ShareOptions{Policy: PolicyEager})
	eventually(t, "the resumed fill to finish", func() bool {
		r := c.Reports()
		return len(r) == 1 && r[0].Done
	})
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, e := range store.applied {
		if e.Path == "main.go" {
			return
		}
	}
	t.Errorf("main.go, changed only here, was not sent; sent %v", store.applied)
}

// A file the workspace cannot read is named and set aside until it changes; the
// files beside it land.
func TestAFileTheWorkspaceCannotReadIsSetAsideAndTheRestLand(t *testing.T) {
	root := t.TempDir()
	store := &faultStore{unreadable: map[string]bool{"/bad": true}}
	store.add("/bad", "x")
	store.add("/good", "y")
	c, logs, clk := wbCache(t, store, root)

	rounds(t, c, clk, 3)

	if !exists(root, "good") {
		t.Error("good did not land beside a file the workspace could not read")
	}
	if exists(root, "bad") {
		t.Error("a file the workspace could not read was written")
	}
	r := c.Reports()
	if len(r) != 1 || len(r[0].WriteBack.Skipped) != 1 || r[0].WriteBack.Skipped[0].Path != "/bad" {
		t.Errorf("status does not name the unreadable file: %+v", r)
	}
	if n := strings.Count(logs.String(), "cannot be carried back"); n != 1 {
		t.Errorf("the unreadable file was named %d times in 3 rounds, want once:\n%s", n, logs.String())
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, later := range store.pulled[1:] {
		if slices.Contains(later, "/bad") {
			t.Errorf("a file set aside was asked for again: %v", store.pulled)
		}
	}
}

// An ephemeral share is never carried back, so there is no both-changed rule to
// protect: the edit here is what the container should get, and it is sent.
func TestAResumedFillOfAnEphemeralShareSendsWhatChangedHere(t *testing.T) {
	root := t.TempDir()
	writeLocal(t, root, "main.go", "edited here since the restart")
	rec := &memRecord{m: map[string]map[string]Baseline{wbShare: {
		"/main.go": {Size: 3, ModTime: time.Unix(5, 0)},
	}}}
	store := &faultStore{}
	store.changes = []Change{{Path: "/main.go", Size: 9, ModTime: time.Now().UnixNano()}}
	c := &Cache{Store: func() (Store, bool) { return store, true }, Record: rec, Ctx: t.Context()}
	t.Cleanup(c.Stop)

	c.Attach(wbShare, root, ShareOptions{Policy: PolicyEager, Ephemeral: true})
	eventually(t, "the resumed fill to finish", func() bool {
		r := c.Reports()
		return len(r) == 1 && r[0].Done
	})
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, e := range store.applied {
		if e.Path == "main.go" {
			return
		}
	}
	t.Errorf("main.go was not sent to an ephemeral share; sent %v", store.applied)
}

// A consumer that cannot say what exists keeps everything, and does not say so
// every round.
func TestAStoreThatCannotPresentIsNotLoggedEveryRound(t *testing.T) {
	root := t.TempDir()
	writeLocal(t, root, "D/old", "x")
	store := &faultStore{changes: []Change{{Path: "/D", Deleted: true}}, presentFn: func([]string) ([]string, error) {
		return nil, ErrCannotPresent
	}}
	c, logs, clk := wbCache(t, store, root)
	c.shares.noteSent(wbShare, root, []Entry{{Path: "D/old"}})

	rounds(t, c, clk, 3)
	if !exists(root, "D/old") {
		t.Error("a file was deleted on a directory's say-so by a store that cannot confirm it")
	}
	if strings.Contains(logs.String(), "asking which paths") {
		t.Errorf("a store that cannot answer was reported as failing:\n%s", logs.String())
	}
}
