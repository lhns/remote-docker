package dircache

// Write-back must never stop for good, and nothing the consumer made may be
// lost or mangled on the way back (ADR 0044). One test per way it could stop.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// faultStore is a Store whose answers a test scripts: what the consumer
// changed, what each file holds, and where a call fails.
type faultStore struct {
	mu sync.Mutex

	changes []Change
	content map[string]string // path -> body
	mtime   time.Time

	changesFn  func() error
	pullFn     func(ctx context.Context, paths []string, deliver func(path string) error) error
	applyFn    func(entries []Entry) error
	presentFn  func(paths []string) ([]string, error)
	skipPaths  map[string]bool // share-relative, no leading slash
	unreadable map[string]bool // with a leading slash: named, never sent

	applied []Entry
	pulled  [][]string
	asked   []string // what Present was asked
}

func (f *faultStore) Apply(_ context.Context, _, root string, entries []Entry, _ bool) (Applied, error) {
	if f.applyFn != nil {
		if err := f.applyFn(entries); err != nil {
			return Applied{}, err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applied = append(f.applied, entries...)
	return Applied{Read: readHere(root, entries), Skipped: f.skip(entries)}, nil
}

// skip names the entries the far end is scripted to leave out.
func (f *faultStore) skip(entries []Entry) []string {
	var out []string
	for _, e := range entries {
		if f.skipPaths[e.Path] {
			out = append(out, "/"+e.Path)
		}
	}
	return out
}

func (f *faultStore) Drop(context.Context, string, []string) error { return nil }

func (f *faultStore) Changes(context.Context, string) ([]Change, error) {
	if f.changesFn != nil {
		if err := f.changesFn(); err != nil {
			return nil, err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Change, len(f.changes))
	copy(out, f.changes)
	return out, nil
}

func (f *faultStore) Pull(ctx context.Context, _ string, paths []string, into func(File) error) error {
	f.mu.Lock()
	f.pulled = append(f.pulled, append([]string(nil), paths...))
	f.mu.Unlock()

	deliver := func(p string) error {
		f.mu.Lock()
		body, ok := f.content[p]
		at := f.mtime
		bad := f.unreadable[p]
		f.mu.Unlock()
		if bad {
			return into(File{Path: p, Unreadable: "input/output error"})
		}
		if !ok {
			return nil
		}
		return into(File{Path: p, ModTime: at, Mode: 0o644, Body: strings.NewReader(body)})
	}
	if f.pullFn != nil {
		return f.pullFn(ctx, paths, deliver)
	}
	for _, p := range paths {
		if err := deliver(p); err != nil {
			return err
		}
	}
	return nil
}

// add makes the consumer have written path with body, newer than anything here.
func (f *faultStore) add(path, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.content == nil {
		f.content = map[string]string{}
	}
	if f.mtime.IsZero() {
		f.mtime = time.Now().Add(time.Minute).Truncate(time.Second)
	}
	f.content[path] = body
	f.changes = append(f.changes, Change{Path: path, Size: int64(len(body)), ModTime: f.mtime.UnixNano()})
}

const wbShare = "/m/1111111111111111"

// wbCache is a cache over store with one complete share at root, logging to
// the returned buffer.
func wbCache(t *testing.T, store Store, root string) (*Cache, *syncBuffer, *fakeClock) {
	t.Helper()
	logs := &syncBuffer{}
	clk := &fakeClock{t: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
	c := &Cache{
		Store:      func() (Store, bool) { return store, true },
		Ctx:        t.Context(),
		Log:        slog.New(slog.NewTextHandler(logs, nil)),
		now:        clk.Now,
		ChangeTime: modTimeAsChangeTime,
	}
	t.Cleanup(c.Stop)
	c.shares.set(wbShare, root, &shareState{Report: Report{Done: true}, Cached: true}, nil)
	return c, logs, clk
}

// fakeClock is the time write-back backs off by, which a test moves instead of
// waiting for.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// rounds runs n write-back rounds, each after more than the longest backoff, so
// a failure never makes the next round wait.
func rounds(t *testing.T, c *Cache, clk *fakeClock, n int) {
	t.Helper()
	for range n {
		clk.advance(2 * writeBackBackoffMax)
		c.writeBackRound(t.Context())
	}
}

func exists(root, rel string) bool {
	_, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
	return err == nil
}

// A pull that runs out of budget half way must keep what it landed: the
// manifest and the record move per file. Left where they were, the files that
// did land are neither compared against nor taken out of the cache when
// deleted here, and a burst bigger than the budget is retried whole for ever.
func TestWriteBackKeepsWhatLandedWhenAPullRunsOutOfBudget(t *testing.T) {
	root := t.TempDir()
	store := &faultStore{}
	for i := range 6 {
		store.add(fmt.Sprintf("/f%d", i), "body")
	}
	var slow sync.Once
	store.pullFn = func(ctx context.Context, paths []string, deliver func(string) error) error {
		stalled := false
		slow.Do(func() { stalled = true })
		for i, p := range paths {
			if stalled && i == 3 {
				<-ctx.Done()
				return ctx.Err()
			}
			if err := deliver(p); err != nil {
				return err
			}
		}
		return nil
	}
	c, _, clk := wbCache(t, store, root)
	c.callBudget = 50 * time.Millisecond

	rounds(t, c, clk, 1)

	base := c.shares.baselines(wbShare)
	recorded := c.shares.recordedSet(wbShare)
	for i := range 3 {
		p := fmt.Sprintf("/f%d", i)
		if !exists(root, p) {
			t.Fatalf("%s was not written before the budget ran out", p)
		}
		if _, ok := base[p]; !ok {
			t.Errorf("%s landed but the manifest does not hold it; baselines = %v", p, base)
		}
		if !recorded[p] {
			t.Errorf("%s landed but is not recorded; record = %v", p, recorded)
		}
	}

	rounds(t, c, clk, 2)
	for i := range 6 {
		if p := fmt.Sprintf("/f%d", i); !exists(root, p) {
			t.Errorf("%s never arrived", p)
		}
	}
}

// One file this machine cannot hold (a name NTFS refuses, with EINVAL as
// the portable stand-in) must not stop the rest, and is named once, not every
// round for ever.
func TestWriteBackSkipsAFileItCannotWriteAndCarriesOn(t *testing.T) {
	root := t.TempDir()
	store := &faultStore{}
	store.add("/a:bad", "x")
	store.add("/b", "y")
	store.add("/c", "z")

	c, logs, clk := wbCache(t, store, root)
	var attempts int
	c.writeFile = func(r *os.Root, f File) error {
		if f.Path == "/a:bad" {
			attempts++
			return &os.PathError{Op: "openat", Path: f.Path, Err: errBadName}
		}
		return writeUnder(r, f, nil, 0)
	}

	rounds(t, c, clk, 5)

	for _, p := range []string{"b", "c"} {
		if !exists(root, p) {
			t.Errorf("%s never arrived behind a file that cannot be written", p)
		}
	}
	if attempts > 1 {
		t.Errorf("the unwritable file was tried %d times in 5 rounds, want it given up on", attempts)
	}
	named := 0
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "a:bad") {
			named++
		}
	}
	if named != 1 {
		t.Errorf("the unwritable file is named on %d log lines, want once:\n%s", named, logs.String())
	}
}

// A full disk is transient: the backlog lands once there is room, and a
// file made after that lands too.
func TestWriteBackResumesAfterTheDiskWasFull(t *testing.T) {
	root := t.TempDir()
	store := &faultStore{}
	for i := range 4 {
		store.add(fmt.Sprintf("/f%d", i), "body")
	}
	c, _, clk := wbCache(t, store, root)
	full := true
	c.writeFile = func(r *os.Root, f File) error {
		if full {
			return &os.PathError{Op: "write", Path: f.Path, Err: errNoSpace}
		}
		return writeUnder(r, f, nil, 0)
	}

	rounds(t, c, clk, 4)
	if exists(root, "f0") {
		t.Fatal("a file landed on a full disk")
	}

	full = false
	store.add("/later", "after")
	rounds(t, c, clk, 12)

	for _, p := range []string{"f0", "f1", "f2", "f3", "later"} {
		if !exists(root, p) {
			t.Errorf("%s did not arrive after the disk had room again", p)
		}
	}
}

// A fill batch that fails once must be retried, never final: with the fill
// stuck, the share is never complete and write-back never runs.
func TestAFailedFillBatchIsRetriedAndWriteBackFollows(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"a.go", "b.go", "c.go"} {
		if err := os.WriteFile(filepath.Join(root, n), []byte("package x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var calls int
	store := &faultStore{}
	store.applyFn = func([]Entry) error {
		calls++
		if calls == 1 {
			return errors.New("the workspace refused the batch")
		}
		return nil
	}
	clk := &fakeClock{t: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
	c := &Cache{Store: func() (Store, bool) { return store, true }, Ctx: t.Context(), now: clk.Now, fillBackoff: 5 * time.Millisecond}
	t.Cleanup(c.Stop)

	c.Attach(wbShare, root, ShareOptions{Policy: PolicyEager})
	eventually(t, "the fill to be retried and finish", func() bool {
		r := c.Reports()
		return len(r) == 1 && r[0].Done && r[0].Err == nil
	})

	store.add("/made-by-the-container", "hello")
	rounds(t, c, clk, 1)
	if !exists(root, "made-by-the-container") {
		t.Error("write-back did not run after the fill recovered")
	}
}

// After a client restart a file the fill sent and the consumer then
// modified must still come back. Only paths survive the restart, so every path
// counts as one the fill never sent, and a modified file is mistaken for one
// this machine may have created.
func TestAModifiedFileIsWrittenBackAfterARestart(t *testing.T) {
	root := t.TempDir()
	local := filepath.Join(root, "main.go")
	if err := os.WriteFile(local, []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := &fakeRecord{}

	first := &Cache{Store: func() (Store, bool) { return &faultStore{}, true }, Record: rec, Ctx: t.Context()}
	t.Cleanup(first.Stop)
	first.Attach(wbShare, root, ShareOptions{Policy: PolicyEager})
	eventually(t, "the first fill to finish", func() bool {
		r := first.Reports()
		return len(r) == 1 && r[0].Done && r[0].Err == nil
	})

	store := &faultStore{}
	store.add("/main.go", "after, from the container\n")
	secondClock := &fakeClock{t: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
	second := &Cache{Store: func() (Store, bool) { return store, true }, Record: rec, Ctx: t.Context(), now: secondClock.Now}
	t.Cleanup(second.Stop)
	second.Attach(wbShare, root, ShareOptions{Policy: PolicyOff})
	rounds(t, second, secondClock, 1)

	got, err := os.ReadFile(local)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "after, from the container\n" {
		t.Errorf("after a restart the file holds %q, want the container's version", got)
	}
}

// The property the rest are instances of. Whatever transient failures come
// first, once they stop every file the container made lands with its content,
// a file this machine cannot hold is skipped without stopping the others, and
// nothing else is written.
func TestWriteBackEventuallyLandsEverythingWhateverFailedFirst(t *testing.T) {
	for seed := uint64(1); seed <= 20; seed++ {
		t.Run(fmt.Sprint("seed ", seed), func(t *testing.T) {
			rng := rand.New(rand.NewPCG(seed, seed*7919))
			root := t.TempDir()
			store := &faultStore{}
			bad := map[string]bool{}
			want := map[string]string{}
			for i := range 12 {
				// Sorted ahead of the rest as often as behind it.
				name := fmt.Sprintf("/d%d/f%02d", i%3, i)
				body := fmt.Sprintf("body %d", i)
				if rng.IntN(4) == 0 {
					name += ":bad"
					bad[name] = true
				} else {
					want[name] = body
				}
				store.add(name, body)
			}

			faulty := true
			store.changesFn = func() error {
				if faulty && rng.IntN(3) == 0 {
					return errors.New("the channel reset")
				}
				return nil
			}
			store.pullFn = func(_ context.Context, paths []string, deliver func(string) error) error {
				for _, p := range paths {
					if faulty && rng.IntN(5) == 0 {
						return errors.New("the channel reset")
					}
					if err := deliver(p); err != nil {
						return err
					}
				}
				return nil
			}
			c, _, clk := wbCache(t, store, root)
			c.writeFile = func(r *os.Root, f File) error {
				if bad[f.Path] {
					return &os.PathError{Op: "openat", Path: f.Path, Err: errBadName}
				}
				if faulty && rng.IntN(4) == 0 {
					return &os.PathError{Op: "write", Path: f.Path, Err: errNoSpace}
				}
				return writeUnder(r, f, nil, 0)
			}

			rounds(t, c, clk, 15)
			faulty = false
			rounds(t, c, clk, 25)

			var found []string
			_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
				if err == nil && !d.IsDir() {
					rel, _ := filepath.Rel(root, p)
					found = append(found, "/"+filepath.ToSlash(rel))
				}
				return nil
			})
			sort.Strings(found)

			var wantNames []string
			for n := range want {
				wantNames = append(wantNames, n)
			}
			sort.Strings(wantNames)
			if fmt.Sprint(found) != fmt.Sprint(wantNames) {
				t.Fatalf("the share holds %v, want exactly %v", found, wantNames)
			}
			base := c.shares.baselines(wbShare)
			for n, body := range want {
				got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(n)))
				if err != nil || string(got) != body {
					t.Errorf("%s holds %q (%v), want %q", n, got, err, body)
				}
				if _, ok := base[n]; !ok {
					t.Errorf("%s landed but the manifest does not hold it", n)
				}
			}
		})
	}
}
