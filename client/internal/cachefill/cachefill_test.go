package cachefill

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// tree writes files of the given sizes and returns the root.
func tree(t *testing.T, files map[string]int) string {
	t.Helper()
	root := t.TempDir()
	for name, size := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func paths(entries []Entry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Path)
	}
	return out
}

// The cache may hold only what the watcher can invalidate. An excluded
// directory is served live instead, which is slower and right.
func TestWalkSkipsWhatTheWatcherDoesNot(t *testing.T) {
	root := tree(t, map[string]int{
		"main.go":             10,
		".git/objects/aaaa":   10,
		"node_modules/x/y.js": 10,
	})

	var found []Entry
	stats := Walk(root, []string{".git", "node_modules"}, func(e Entry) { found = append(found, e) })

	if got := paths(found); len(got) != 1 || got[0] != "main.go" {
		t.Errorf("walked %v, want only main.go", got)
	}
	if stats.Excluded != 2 {
		t.Errorf("Excluded = %d, want the two directories", stats.Excluded)
	}
}

// A directory holds no bytes of its own and travels with the files inside it;
// what a tar cannot carry to another machine is not cached at all.
func TestWalkYieldsOnlyRegularFiles(t *testing.T) {
	root := tree(t, map[string]int{"pkg/deep/file.go": 8})
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	var found []Entry
	Walk(root, nil, func(e Entry) { found = append(found, e) })
	if got := paths(found); len(got) != 1 || got[0] != "pkg/deep/file.go" {
		t.Errorf("walked %v, want the one file", got)
	}
}

// The walk does NOT apply the budget, and that is the point: one that stopped
// at the ceiling would let the directory order decide what a share caches --
// the first files it reached, however large, and never the small ones behind
// them. Choosing needs to see the candidates, so the Selector holds the budget.
func TestWalkYieldsEverythingAndLetsTheSelectorChoose(t *testing.T) {
	root := tree(t, map[string]int{"a.bin": 900, "b.go": 10, "c.go": 10})

	var found []Entry
	stats := Walk(root, nil, func(e Entry) { found = append(found, e) })

	if len(found) != 3 || stats.TotalFiles != 3 {
		t.Errorf("walked %v with stats %+v, want all three", paths(found), stats)
	}
}

// The buffer keeps the smallest files seen so far, so the upload can start
// before the scan ends and still send the cheapest first.
func TestSelectorKeepsTheSmallest(t *testing.T) {
	s := NewSelector(Budget{Files: 3, Bytes: 1 << 20})

	for _, size := range []int64{500, 10, 900, 20, 700, 30} {
		s.Add(Entry{Path: "f", Size: size})
	}

	got := s.TakeSmallest(1<<20, 100)
	if len(got) != 3 {
		t.Fatalf("took %d entries, want the three that fit", len(got))
	}
	for i, want := range []int64{10, 20, 30} {
		if got[i].Size != want {
			t.Errorf("took %v, want the three smallest ascending", sizes(got))
			break
		}
	}
}

// Bounded by BYTES as well as count, and evicting the largest is what makes
// room: a tree of tiny files must not put millions of entries in memory before
// the byte ceiling is anywhere near.
func TestSelectorEvictsTheLargest(t *testing.T) {
	s := NewSelector(Budget{Files: 100, Bytes: 20})

	// big fits when it arrives, so it is taken; the two that follow do not fit
	// beside it, and it is what makes room for them.
	s.Add(Entry{Path: "big", Size: 15})
	s.Add(Entry{Path: "small", Size: 4})
	s.Add(Entry{Path: "smaller", Size: 3})

	got := s.TakeSmallest(1000, 100)
	if len(got) != 2 || got[0].Path != "smaller" || got[1].Path != "small" {
		t.Errorf("kept %v, want the two small ones after evicting big", paths(got))
	}
}

// A file that cannot fit and is larger than everything held is refused rather
// than evicting something smaller to make room for it.
func TestSelectorRefusesAWorseCandidate(t *testing.T) {
	s := NewSelector(Budget{Files: 1, Bytes: 1000})
	s.Add(Entry{Path: "small", Size: 10})

	if s.Add(Entry{Path: "huge", Size: 900}) {
		t.Error("a larger file displaced a smaller one")
	}
	if got := s.TakeSmallest(1000, 100); len(got) != 1 || got[0].Path != "small" {
		t.Errorf("kept %v, want the small one", paths(got))
	}
}

// What has already gone counts against the budget: the ceiling is on what a
// share caches in total, not on what happens to be waiting.
func TestSelectorCountsWhatItHasSent(t *testing.T) {
	s := NewSelector(Budget{Files: 2, Bytes: 1000})

	s.Add(Entry{Path: "a", Size: 10})
	if got := s.TakeSmallest(1000, 100); len(got) != 1 {
		t.Fatalf("took %v", paths(got))
	}
	if files, bytes := s.Sent(); files != 1 || bytes != 10 {
		t.Errorf("Sent() = %d, %d; want 1, 10", files, bytes)
	}

	s.Add(Entry{Path: "b", Size: 10})
	if s.Add(Entry{Path: "c", Size: 10}) {
		t.Error("the budget was exceeded by counting only what is waiting")
	}
}

func sizes(entries []Entry) []int64 {
	out := make([]int64, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Size)
	}
	return out
}

// collect runs a stream and records the batches it sent.
func collect(t *testing.T, root string, budget Budget) ([][]Entry, Stats) {
	t.Helper()
	var batches [][]Entry

	done := make(chan struct{})
	var (
		stats Stats
		err   error
	)
	go func() {
		defer close(done)
		stats, err = Stream(root, nil, budget, func(b []Entry) error {
			batches = append(batches, append([]Entry(nil), b...))
			return nil
		})
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the fill never finished; a tree that never wakes the drain is a fill that never uploads")
	}
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	return batches, stats
}

func sent(batches [][]Entry) []string {
	var out []string
	for _, b := range batches {
		out = append(out, paths(b)...)
	}
	return out
}

// A tree smaller than the sample never reaches the count that wakes the drain
// while scanning, so the end of the scan has to wake it. Without that, the
// commonest case of all -- a small project -- would upload nothing at all and
// hang.
func TestStreamUploadsATreeSmallerThanTheSample(t *testing.T) {
	files := map[string]int{}
	for i := range Sample / 4 {
		files[fmt.Sprintf("f%d.go", i)] = 10
	}
	root := tree(t, files)

	batches, stats := collect(t, root, Budget{})

	if got := len(sent(batches)); got != len(files) {
		t.Errorf("sent %d files, want all %d", got, len(files))
	}
	if !stats.Complete() {
		t.Errorf("stats = %+v, want complete", stats)
	}
}

// The degenerate cases, which are the ones that hang if the wake-up is wrong.
func TestStreamFinishesWithNothingToSend(t *testing.T) {
	t.Run("an empty directory", func(t *testing.T) {
		batches, stats := collect(t, t.TempDir(), Budget{})
		if len(batches) != 0 {
			t.Errorf("sent %v from an empty tree", sent(batches))
		}
		if stats.TotalFiles != 0 {
			t.Errorf("stats = %+v", stats)
		}
	})

	t.Run("a single file", func(t *testing.T) {
		batches, _ := collect(t, tree(t, map[string]int{"only.go": 4}), Budget{})
		if got := sent(batches); len(got) != 1 || got[0] != "only.go" {
			t.Errorf("sent %v, want the one file", got)
		}
	})

	t.Run("a directory that is not there", func(t *testing.T) {
		batches, stats := collect(t, filepath.Join(t.TempDir(), "absent"), Budget{})
		if len(batches) != 0 || stats.TotalFiles != 0 {
			t.Errorf("sent %v with stats %+v", sent(batches), stats)
		}
	})
}

// Exactly the sample, which is the boundary where the drain is woken by the
// count for the first and only time.
func TestStreamAtExactlyTheSample(t *testing.T) {
	files := map[string]int{}
	for i := range Sample {
		files[fmt.Sprintf("f%d.go", i)] = 8
	}

	batches, stats := collect(t, tree(t, files), Budget{})
	if got := len(sent(batches)); got != Sample {
		t.Errorf("sent %d files, want %d", got, Sample)
	}
	if !stats.Complete() {
		t.Errorf("stats = %+v, want complete", stats)
	}
}

// Over the budget the smallest are sent and the rest are left to the live
// export. Nothing fails, and the fill still finishes.
func TestStreamStopsAtTheBudget(t *testing.T) {
	files := map[string]int{}
	for i := range 50 {
		files[fmt.Sprintf("small%02d.go", i)] = 10
	}
	files["big.bin"] = 100000

	batches, stats := collect(t, tree(t, files), Budget{Files: 10})

	got := sent(batches)
	if len(got) != 10 {
		t.Fatalf("sent %d files, want the 10 that fit", len(got))
	}
	for _, p := range got {
		if p == "big.bin" {
			t.Error("the largest file was sent while smaller ones were not")
		}
	}
	if stats.Complete() {
		t.Error("a partly cached share reported itself complete")
	}
}

// A send that fails stops the fill rather than spinning: the share is served
// from the live export meanwhile, and there is nothing useful left to do.
func TestStreamStopsWhenSendingFails(t *testing.T) {
	files := map[string]int{}
	for i := range Sample * 3 {
		files[fmt.Sprintf("f%04d.go", i)] = 10
	}

	calls := 0
	_, err := Stream(tree(t, files), nil, Budget{}, func([]Entry) error {
		calls++
		return errors.New("the workspace went away")
	})
	if err == nil {
		t.Fatal("a failing send was not reported")
	}
	if calls != 1 {
		t.Errorf("send was called %d times, want it to stop at the first failure", calls)
	}
}

// Batches arrive cheapest first, which is the whole ordering policy.
func TestStreamSendsTheCheapestFirst(t *testing.T) {
	files := map[string]int{"huge.bin": 40000, "mid.txt": 4000}
	for i := range Sample {
		files[fmt.Sprintf("tiny%03d.go", i)] = 4
	}

	batches, _ := collect(t, tree(t, files), Budget{})

	var last int64
	for _, b := range batches {
		for _, e := range b {
			if e.Size < last {
				t.Fatalf("a %d-byte file followed a %d-byte one; batches must be ascending",
					e.Size, last)
			}
			last = e.Size
		}
	}
}
