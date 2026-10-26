package dircache

// A copy this machine sent is never the consumer's change (ADR 0044), however
// the far end stamps it and however the file here moves while it is sent. The
// far end's own record of what it applied is not what this rests on: it is
// lost when the agent restarts, and these stores list every file they hold.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// landingStore keeps what it is sent the way the workspace does: the time is
// carried to the second, as a tar carries it, and Changes lists every file
// it holds, as an agent that lost its record of what it applied does.
type landingStore struct {
	mu     sync.Mutex
	files  map[string]landedFile // with a leading slash
	during func(rel string)      // runs once a file has been read, before it lands
}

type landedFile struct {
	body    string
	mtime   time.Time
	changed time.Time
}

func (s *landingStore) Apply(_ context.Context, _, root string, entries []Entry, _ bool) (Applied, error) {
	var applied Applied
	for _, e := range entries {
		full := filepath.Join(root, filepath.FromSlash(e.Path))
		info, err := os.Stat(full)
		if err != nil {
			continue
		}
		body, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		applied.Read = append(applied.Read, Read{Path: e.Path, Size: info.Size(), ModTime: info.ModTime()})
		if s.during != nil {
			s.during(e.Path)
		}
		s.mu.Lock()
		if s.files == nil {
			s.files = map[string]landedFile{}
		}
		s.files["/"+e.Path] = landedFile{body: string(body), mtime: info.ModTime().Round(time.Second), changed: time.Now()}
		s.mu.Unlock()
	}
	return applied, nil
}

func (s *landingStore) Drop(_ context.Context, _ string, paths []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range paths {
		delete(s.files, slashed(p))
	}
	return nil
}

func (s *landingStore) Changes(context.Context, string) ([]Change, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Change
	for p, f := range s.files {
		out = append(out, Change{
			Path: p, Size: int64(len(f.body)), ModTime: f.mtime.UnixNano(),
			Changed: f.changed.UnixNano(), Mode: 0o644,
		})
	}
	return out, nil
}

func (s *landingStore) Pull(_ context.Context, _ string, paths []string, into func(File) error) error {
	for _, p := range paths {
		s.mu.Lock()
		f, ok := s.files[p]
		s.mu.Unlock()
		if !ok {
			continue
		}
		if err := into(File{Path: p, ModTime: f.mtime, Mode: 0o644, Body: strings.NewReader(f.body)}); err != nil {
			return err
		}
	}
	return nil
}

// sendHere is what the watcher does for an edit here: invalidation sends it.
func sendHere(t *testing.T, c *Cache, root string, paths ...string) {
	t.Helper()
	pending := map[string]pend{}
	for _, p := range paths {
		pending[slashed(p)] = pend{}
	}
	c.invalidate(t.Context(), wbShare, root, pending)
}

// subSecond is a time with nanoseconds in it, as almost every file has.
func subSecond(at time.Time) time.Time {
	return at.Truncate(time.Second).Add(123456789)
}

// A file sent into the cache and then deleted here stays deleted: the cache's
// copy is what this machine sent, stamped to the second, and not a change the
// container made that a deletion here would have to give way to.
func TestADeletionHereIsNotUndoneByTheCopyThisMachineSent(t *testing.T) {
	root := t.TempDir()
	writeLocal(t, root, "marker", "edited here\n")
	setTime(t, root, "marker", subSecond(time.Now().Add(-time.Minute)))

	store := &landingStore{}
	c, logs, clk := wbCache(t, store, root)
	sendHere(t, c, root, "marker")

	if err := os.Remove(filepath.Join(root, "marker")); err != nil {
		t.Fatal(err)
	}
	rounds(t, c, clk, 2)

	if exists(root, "marker") {
		t.Errorf("a file deleted here came back from the copy this machine sent; log:\n%s", logs)
	}
}

// A file rewritten here WHILE it is being sent keeps the newer version. What
// landed is the version read; a baseline taken from the file afterwards
// describes the rewrite, and the copy that landed then reads as the container's
// change, carried back over the rewrite.
func TestAFileRewrittenWhileItIsSentKeepsTheRewrite(t *testing.T) {
	root := t.TempDir()
	writeLocal(t, root, "churn.txt", "old\n")
	setTime(t, root, "churn.txt", subSecond(time.Now().Add(-time.Minute)))

	var once sync.Once
	store := &landingStore{during: func(rel string) {
		once.Do(func() {
			writeLocal(t, root, rel, "rewritten while it was sent\n")
			setTime(t, root, rel, subSecond(time.Now()))
		})
	}}
	c, logs, clk := wbCache(t, store, root)
	sendHere(t, c, root, "churn.txt")
	rounds(t, c, clk, 2)

	if got, _ := os.ReadFile(filepath.Join(root, "churn.txt")); string(got) != "rewritten while it was sent\n" {
		t.Errorf("churn.txt holds %q, want the rewrite made here; log:\n%s", got, logs)
	}
}
