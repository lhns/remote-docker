package nfsserve

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/osfs"
)

// countingOpens counts the opens that reach the filesystem underneath the
// cache, which is the whole measurement (fdcache.go).
type countingOpens struct {
	billy.Filesystem
	opens atomic.Int64
}

func (c *countingOpens) OpenFile(name string, flag int, perm os.FileMode) (billy.File, error) {
	c.opens.Add(1)
	return c.Filesystem.OpenFile(name, flag, perm)
}

func cachedOver(t *testing.T, dir string, idle time.Duration) (billy.Filesystem, *countingOpens) {
	t.Helper()
	counted := &countingOpens{Filesystem: osfs.New(dir, osfs.WithBoundOS())}
	return withFDCache(counted, idle, fdCacheMax), counted
}

// The point of the cache: the requests that write one file share a descriptor.
func TestADescriptorIsReusedAcrossRequests(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "big.bin"), make([]byte, 8), 0o644); err != nil {
		t.Fatal(err)
	}
	fs, counted := cachedOver(t, dir, 2*time.Second)

	// Five requests, each opening and closing as go-nfs does per WRITE.
	for range 5 {
		f, err := fs.OpenFile("big.bin", os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("OpenFile: %v", err)
		}
		if _, err := f.Write([]byte("x")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}

	if got := counted.opens.Load(); got != 1 {
		t.Errorf("five requests opened the file %d times, want 1", got)
	}
}

// The condition for holding a descriptor at all: a user's own `rm` must keep
// working on a file their container has just written. See openShared in
// fdopen_windows.go for what Go's os.OpenFile does instead.
func TestAHeldDescriptorDoesNotBlockTheMachinesOwnTools(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"doomed.bin", "moved.bin"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("hi"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fs, _ := cachedOver(t, dir, time.Minute) // long, so the descriptor is still held

	for _, name := range []string{"doomed.bin", "moved.bin"} {
		f, err := fs.OpenFile(name, os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("OpenFile(%s): %v", name, err)
		}
		if err := f.Close(); err != nil { // returns it to the cache, still open
			t.Fatal(err)
		}
	}

	if err := os.Remove(filepath.Join(dir, "doomed.bin")); err != nil {
		t.Errorf("deleting a file the cache holds open: %v", err)
	}
	if err := os.Rename(filepath.Join(dir, "moved.bin"), filepath.Join(dir, "elsewhere.bin")); err != nil {
		t.Errorf("renaming a file the cache holds open: %v", err)
	}
}

// Windows refuses to rename a directory holding an open file even when the
// file itself was opened to allow it, so a rename drops the descriptors below
// it as well as its own. See fdCacheFS.Remove.
func TestRenamingADirectoryDropsTheDescriptorsBelowIt(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "a", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a", "deep", "b"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	fs, _ := cachedOver(t, dir, time.Minute)

	f, err := fs.OpenFile(filepath.Join("a", "deep", "b"), os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if err := fs.Rename("a", "e"); err != nil {
		t.Errorf("renaming a directory holding a cached descriptor: %v", err)
	}
}

// Off when asked, because a setting that cannot be turned off is not one.
func TestTheCacheIsOnByDefaultAndCanBeTurnedOff(t *testing.T) {
	for _, row := range []struct {
		env  string
		set  bool
		want time.Duration
	}{
		{set: false, want: 2 * time.Second},
		{env: "", set: true, want: 2 * time.Second},
		{env: "5s", set: true, want: 5 * time.Second},
		{env: "0", set: true, want: 0},
		{env: "nonsense", set: true, want: 2 * time.Second},
	} {
		if row.set {
			t.Setenv("REMOTE_DOCKER_NFS_FDCACHE", row.env)
		} else {
			unsetEnv(t, "REMOTE_DOCKER_NFS_FDCACHE")
		}
		if got := fdCacheIdle(); got != row.want {
			t.Errorf("REMOTE_DOCKER_NFS_FDCACHE=%q (set=%v) gives %s, want %s", row.env, row.set, got, row.want)
		}
	}
}

// A descriptor evicted while a request still holds it must be closed when that
// request lets go.
//
// Eviction drops the entry from the map so no later request finds a descriptor
// about to stop meaning its name. That leaves the last release as the only
// thing that can close it, and it used to set an idle timer that looked the
// entry up by name instead — finding nothing, because the eviction had already
// removed it. The descriptor stayed open for the life of the process,
// unreachable, holding the file open on Windows.
func TestADescriptorEvictedWhileInUseIsClosedByItsLastRelease(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.bin"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	fs, _ := cachedOver(t, dir, time.Minute)
	cache := fs.(*fdCacheFS)

	f, err := fs.OpenFile("f.bin", os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}

	cache.mu.Lock()
	held := cache.open["f.bin"]
	cache.mu.Unlock()
	if held == nil {
		t.Fatal("the descriptor was not cached, so this test would prove nothing")
	}

	// Removing the name evicts it while the request still holds it.
	if err := fs.Remove("f.bin"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// An open descriptor stats; a closed one cannot. Not compared against
	// os.ErrClosed: openShared builds the file with os.NewFile on Windows, so
	// a closed handle answers with the platform's own error instead.
	if _, err := held.file.Stat(); err == nil {
		t.Error("the evicted descriptor is still open after its last release")
	}
}

// The idle timer must close the entry it was set for, not whatever holds that
// name when it fires: a name reopened in between belongs to a live request.
func TestTheIdleTimerClosesItsOwnDescriptor(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.bin"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	fs, _ := cachedOver(t, dir, time.Minute)
	cache := fs.(*fdCacheFS)

	first, err := fs.OpenFile("f.bin", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	cache.mu.Lock()
	stale := cache.open["f.bin"]
	cache.mu.Unlock()

	cache.evict("f.bin") // the name is dropped, first still holds the descriptor
	_ = first.Close()    // and closes it

	second, err := fs.OpenFile("f.bin", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()

	cache.mu.Lock()
	live := cache.open["f.bin"]
	cache.mu.Unlock()
	if live == nil || live == stale {
		t.Fatal("the reopen did not produce a new entry, so this test would prove nothing")
	}

	// Firing the first entry's eviction must leave the second alone.
	cache.evictEntry(stale)
	if _, err := live.file.Stat(); err != nil {
		t.Errorf("the live descriptor was closed by another entry's eviction: %v", err)
	}
}
