package nfsserve

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5"

	"github.com/lhns/remote-docker/core/workspace"
)

// fdCacheIn reports the descriptor cache inside a share's filesystem, and nil
// when REMOTE_DOCKER_NFS_FDCACHE turned it off. Anywhere rather than
// outermost, the way tracedLayer walks: attributes sit above it, and a
// single-file share adds another layer.
func fdCacheIn(fs billy.Filesystem) *fdCacheFS {
	for {
		switch v := fs.(type) {
		case *fdCacheFS:
			return v
		case *singleFileFS:
			fs = v.Filesystem
		case *attrFS:
			fs = v.Filesystem
		case *traceFS:
			fs = v.Filesystem
		default:
			return nil
		}
	}
}

// onlyEntry is the one descriptor the cache holds.
func onlyEntry(t *testing.T, c *fdCacheFS) *cachedFD {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.open) != 1 {
		t.Fatalf("the cache holds %d descriptors, want 1", len(c.open))
	}
	for _, e := range c.open {
		return e
	}
	return nil
}

// SetAttrs rebuilds every share's filesystem, and the stack it replaces holds
// real descriptors. Left behind, an orphaned cache keeps a file the container
// is writing open until its idle timers expire, which is the cost the cache
// exists to avoid on Windows (fdcache.go).
func TestSetAttrsClosesTheOutgoingDescriptorCache(t *testing.T) {
	// Long enough that nothing here can be closed by an idle timer instead.
	t.Setenv("REMOTE_DOCKER_NFS_FDCACHE", "1h")

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "held.bin"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := registryFor(t, dir)
	share, _, ok := r.Lookup(workspace.ExportCWD)
	if !ok {
		t.Fatal("the working directory share is not registered")
	}
	cache := fdCacheIn(share.fs)
	if cache == nil {
		t.Fatal("the share has no descriptor cache")
	}

	f, err := share.fs.OpenFile("held.bin", os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := f.Write([]byte("x")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	held := onlyEntry(t, cache)

	// One connect: the uid is known and every share is rebuilt.
	r.SetAttrs(DefaultAttrs)

	// Read rather than Stat: on Windows a Stat goes to the handle and reports
	// the OS error, while every read and write path answers os.ErrClosed.
	if _, err := held.file.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
		t.Errorf("the descriptor the replaced share held answers Read with %v, want %v", err, os.ErrClosed)
	}
	cache.mu.Lock()
	n := len(cache.open)
	cache.mu.Unlock()
	if n != 0 {
		t.Errorf("the replaced share's cache still holds %d descriptors, want 0", n)
	}
}

// Close must not take a descriptor away from a request that is still using it:
// a write mid-flight would land on a closed file. The last release closes it
// instead, which is the path an eviction while in use already takes.
func TestCloseLeavesADescriptorInUseAlone(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "busy.bin"), make([]byte, 8), 0o644); err != nil {
		t.Fatal(err)
	}
	fs, _ := cachedOver(t, dir, time.Hour)
	cache := fs.(*fdCacheFS)

	f, err := fs.OpenFile("busy.bin", os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	held := onlyEntry(t, cache)

	if err := cache.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := f.Write([]byte("x")); err != nil {
		t.Errorf("a write through a descriptor held across Close: %v", err)
	}

	// A request opening now is served uncached rather than refused, and adds
	// nothing that would never be closed.
	after, err := fs.OpenFile("busy.bin", os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("OpenFile after Close: %v", err)
	}
	if err := after.Close(); err != nil {
		t.Fatalf("Close of the uncached file: %v", err)
	}
	cache.mu.Lock()
	n, timer := len(cache.open), held.timer
	cache.mu.Unlock()
	if n != 0 {
		t.Errorf("the closed cache holds %d descriptors, want 0", n)
	}
	if timer != nil {
		t.Error("a closed cache still has an idle timer to fire")
	}

	// The last release closes it, there being nothing left to reuse it.
	if err := f.Close(); err != nil {
		t.Fatalf("Close of the held file: %v", err)
	}
	if _, err := held.file.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
		t.Errorf("the descriptor answers Read with %v after its last holder let go, want %v", err, os.ErrClosed)
	}
}

// Both optional layers are absent when their switch says no (Registry.shareFS),
// so a rebuild has to tolerate a stack with no cache in it at all.
func TestSetAttrsRebuildsWithLayersAbsent(t *testing.T) {
	for _, tc := range []struct{ name, fdcache, trace string }{
		{"cache and tracer off", "0", ""},
		{"cache off, tracer on", "0", "1s"},
		{"cache on, tracer off", "1h", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("REMOTE_DOCKER_NFS_FDCACHE", tc.fdcache)
			if tc.trace == "" {
				unsetEnv(t, "REMOTE_DOCKER_NFS_TRACE")
			} else {
				t.Setenv("REMOTE_DOCKER_NFS_TRACE", tc.trace)
			}

			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "kept.txt"), []byte("content"), 0o644); err != nil {
				t.Fatal(err)
			}
			r := registryFor(t, dir)
			share, _, ok := r.Lookup(workspace.ExportCWD)
			if !ok {
				t.Fatal("the working directory share is not registered")
			}

			r.SetAttrs(DefaultAttrs)

			f, err := share.fs.Open("kept.txt")
			if err != nil {
				t.Fatalf("Open after SetAttrs: %v", err)
			}
			defer func() { _ = f.Close() }()
			got := make([]byte, 7)
			if _, err := f.Read(got); err != nil {
				t.Fatalf("Read after SetAttrs: %v", err)
			}
			if string(got) != "content" {
				t.Errorf("the rebuilt share reads %q, want %q", got, "content")
			}
		})
	}
}
