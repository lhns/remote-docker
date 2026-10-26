package unions

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// A prefetch batch never overwrites what the container made: the copy-up of
// the very file a container is writing can set off the batch that carries the
// older version (ADR 0044). A file the cache layer holds that the client did
// not put there is left and named; one the client put there, and a file the
// layer does not hold, are written as before.
func TestAFillLeavesWhatTheContainerMade(t *testing.T) {
	root, upper := t.TempDir(), t.TempDir()
	write := func(dir, rel, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The union's merged view and its layer are two directories here; what
	// matters is that the layer says who wrote what.
	write(root, "theirs.txt", "the container's")
	write(upper, "theirs.txt", "the container's")
	write(root, "ours.txt", "sent before")
	write(upper, "ours.txt", "sent before")

	var l live
	info, err := os.Stat(filepath.Join(upper, "ours.txt"))
	if err != nil {
		t.Fatal(err)
	}
	l.noteApplied("/ours.txt", applied{size: info.Size(), modTime: info.ModTime(), ctime: ctimeOf(info)})

	up, err := os.OpenRoot(upper)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = up.Close() }()

	_, kept, err := extractWith(root, batchOf(t, "theirs.txt", "ours.txt", "new.txt"), true, containersOwn(up, &l), func(string, os.FileInfo) {})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(kept, []string{"/theirs.txt"}) {
		t.Errorf("kept %v, want only the container's file", kept)
	}
	for rel, want := range map[string]string{"theirs.txt": "the container's", "ours.txt": "ok", "new.txt": "ok"} {
		if got, _ := os.ReadFile(filepath.Join(root, rel)); string(got) != want {
			t.Errorf("%s holds %q, want %q", rel, got, want)
		}
	}
}
