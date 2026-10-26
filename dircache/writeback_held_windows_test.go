//go:build windows

package dircache

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Windows refuses to replace a file another program holds open without
// share-delete. The round must leave the file alone and keep the change pending,
// then land it once the program lets go: never fall back to truncating.
func TestWriteBackRetriesAFileAnotherProgramHolds(t *testing.T) {
	root := t.TempDir()
	local := filepath.Join(root, "main.go")
	if err := os.WriteFile(local, []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(local)
	if err != nil {
		t.Fatal(err)
	}
	changedAt := info.ModTime().Add(time.Minute)

	const share = "/m/1111111111111111"
	store := &fakeStore{
		changes: []Change{{Path: "/main.go", Size: 6, ModTime: changedAt.UnixNano()}},
		files: map[string]File{
			"/main.go": {Path: "/main.go", ModTime: changedAt, Mode: 0o644, Body: strings.NewReader("after\n")},
		},
	}
	c := cacheWith(t, store)
	clk := &fakeClock{t: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
	c.now = clk.Now
	c.shares.set(share, root, &shareState{Report: Report{Done: true}, Cached: true}, nil)
	c.shares.noteSent(share, readHere(root, []Entry{{Path: "main.go", Size: 7}}))

	held, err := os.Open(local) // Go opens without FILE_SHARE_DELETE
	if err != nil {
		t.Fatal(err)
	}
	c.writeBackShare(t.Context(), share, false)

	got, _ := os.ReadFile(local)
	if string(got) != "before\n" {
		t.Errorf("a held file holds %q after a round, want it untouched", got)
	}
	if base := c.shares.baselines(share)["/main.go"]; base.Size != 7 {
		t.Errorf("the baseline moved to %d bytes although nothing landed", base.Size)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 1 {
		t.Errorf("the share holds %v, want only main.go", entries)
	}

	_ = held.Close()
	// The failed round is backed off from, which a test does not wait out.
	clk.advance(time.Hour)
	store.files["/main.go"] = File{Path: "/main.go", ModTime: changedAt, Mode: 0o644, Body: strings.NewReader("after\n")}
	c.writeBackShare(t.Context(), share, false)

	got, _ = os.ReadFile(local)
	if string(got) != "after\n" {
		t.Errorf("the file holds %q once released, want the consumer's version", got)
	}
}
