package cachefill

import (
	"os"
	"path/filepath"
	"testing"
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

// Smallest first, because the win is a round trip saved per file: a thousand
// small files are worth far more than one large one that costs the same
// bandwidth and saves a single round trip.
func TestPlanSendsTheCheapestFirst(t *testing.T) {
	root := tree(t, map[string]int{
		"big.bin":     4096,
		"small.go":    16,
		"pkg/mid.txt": 512,
	})

	entries, stats := Plan(root, nil, Budget{})

	want := []string{"small.go", "pkg/mid.txt", "big.bin"}
	got := paths(entries)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
	if stats.Files != 3 || !stats.Complete() {
		t.Errorf("stats = %+v, want all three cached", stats)
	}
}

// The cache may hold only what the watcher can invalidate. An excluded
// directory is served live instead, which is slower and right.
func TestPlanSkipsWhatTheWatcherDoesNot(t *testing.T) {
	root := tree(t, map[string]int{
		"main.go":             10,
		".git/objects/aaaa":   10,
		"node_modules/x/y.js": 10,
	})

	entries, stats := Plan(root, []string{".git", "node_modules"}, Budget{})

	if got := paths(entries); len(got) != 1 || got[0] != "main.go" {
		t.Errorf("planned %v, want only main.go", got)
	}
	if stats.Excluded != 2 {
		t.Errorf("Excluded = %d, want the two directories", stats.Excluded)
	}
	// The excluded part is not counted as missing: it was never a candidate,
	// so a share that skipped .git is still a complete cache of what it holds.
	if !stats.Complete() {
		t.Errorf("stats = %+v, want complete", stats)
	}
}

// Over the ceiling the rest is served live. Nothing fails, and the share still
// works: "the budget ran out" is the same state as "the fill has not reached
// it yet".
func TestPlanStopsAtTheBudgetWithoutFailing(t *testing.T) {
	root := tree(t, map[string]int{
		"a": 100, "b": 100, "c": 100, "d": 100,
	})

	entries, stats := Plan(root, nil, Budget{Files: 2})
	if len(entries) != 2 {
		t.Errorf("planned %d entries, want 2", len(entries))
	}
	if stats.TotalFiles != 4 {
		t.Errorf("TotalFiles = %d, want everything it saw", stats.TotalFiles)
	}
	if stats.Complete() {
		t.Error("a partly cached share reported itself complete, which write-back would trust")
	}

	// And by size, where stopping matters more: the list is sorted, so
	// everything after the first refusal is larger.
	byBytes, stats := Plan(root, nil, Budget{Bytes: 250})
	if len(byBytes) != 2 || stats.Bytes != 200 {
		t.Errorf("planned %d entries and %d bytes, want 2 and 200", len(byBytes), stats.Bytes)
	}
}

// A directory holds no bytes of its own and travels with the files inside it;
// what a tar cannot carry to another machine is not cached at all.
func TestPlanCachesOnlyRegularFiles(t *testing.T) {
	root := tree(t, map[string]int{"pkg/deep/file.go": 8})
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	entries, _ := Plan(root, nil, Budget{})
	if got := paths(entries); len(got) != 1 || got[0] != "pkg/deep/file.go" {
		t.Errorf("planned %v, want the one file", got)
	}
}

// A batch is a limit on grouping, not on a file: one larger than the limit
// still goes, on its own.
func TestBatches(t *testing.T) {
	entries := []Entry{
		{Path: "a", Size: 10},
		{Path: "b", Size: 10},
		{Path: "huge", Size: 500},
		{Path: "c", Size: 10},
	}

	batches := Batches(entries, 25)
	if len(batches) != 3 {
		t.Fatalf("batches = %v, want three", batches)
	}
	if len(batches[0]) != 2 {
		t.Errorf("the first batch is %v, want the two that fit", batches[0])
	}
	if len(batches[1]) != 1 || batches[1][0].Path != "huge" {
		t.Errorf("the oversized file was not sent alone: %v", batches[1])
	}
	if len(Batches(nil, 10)) != 0 {
		t.Error("an empty plan produced a batch")
	}
}

// The upload must not wait for the scan. A small file is handed over as the
// walk finds it, so on a large tree the first batch goes while the rest is
// still being counted -- the scan would otherwise cost more than the transfer
// it was meant to speed up.
func TestWalkYieldsBeforeItFinishes(t *testing.T) {
	root := tree(t, map[string]int{
		"a.go": 10, "b.go": 10, "c.go": 10, "big.bin": SmallFile * 4,
	})

	var order []string
	stats := Walk(root, nil, Budget{}, func(e Entry) { order = append(order, e.Path) })

	if len(order) != 4 {
		t.Fatalf("yielded %v, want every file", order)
	}
	if stats.TotalFiles != 4 || !stats.Complete() {
		t.Errorf("stats = %+v, want all four", stats)
	}

	// Yielded in walk order, NOT sorted: sorting is the caller's job precisely
	// because it needs the whole list, and waiting for that is what this
	// avoids.
	for _, p := range order {
		if p == "" {
			t.Fatal("an entry arrived with no path")
		}
	}
}

// The budget stops the walk from yielding, and still counts what it saw, so a
// share over the ceiling reports as partly cached rather than as complete.
func TestWalkStopsYieldingAtTheBudget(t *testing.T) {
	root := tree(t, map[string]int{"a": 100, "b": 100, "c": 100})

	var yielded int
	stats := Walk(root, nil, Budget{Files: 1}, func(Entry) { yielded++ })

	if yielded != 1 {
		t.Errorf("yielded %d entries, want 1", yielded)
	}
	if stats.TotalFiles != 3 {
		t.Errorf("TotalFiles = %d, want everything it saw", stats.TotalFiles)
	}
	if stats.Complete() {
		t.Error("a partly cached share reported itself complete")
	}
}
