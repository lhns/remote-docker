package nfsserve

import (
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/lhns/remote-docker/core/workspace"
)

type readLog struct {
	mu    sync.Mutex
	bytes map[string]int64 // "export path" -> bytes
}

func (l *readLog) observe(export, p string, n int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.bytes == nil {
		l.bytes = map[string]int64{}
	}
	l.bytes[export+" "+p] += n
}

func (l *readLog) get(export, p string) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.bytes[export+" "+p]
}

// A read through a share is reported against the share and the file, in bytes
// and in the spelling the rest of the protocol uses, from the root and from
// inside a chroot alike.
func TestReadsAreReportedByShareAndPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte("package p\n\nfunc F() {}\n")
	if err := os.WriteFile(filepath.Join(dir, "pkg", "f.go"), body, 0o644); err != nil {
		t.Fatal(err)
	}

	log := &readLog{}
	r := NewRegistry(DefaultAttrs)
	r.OnRead = log.observe
	if _, err := r.Register(dir, workspace.DefaultMode, workspace.ShareWords{}); err != nil {
		t.Fatal(err)
	}
	share, _, ok := r.Lookup(exportOf(dir))
	if !ok {
		t.Fatal("share not registered")
	}

	// From the share root, the way LOOKUP+READ reaches a file.
	f, err := share.fs.Open(filepath.Join("pkg", "f.go"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(f); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if got := log.get(exportOf(dir), "/pkg/f.go"); got != int64(len(body)) {
		t.Errorf("read of pkg/f.go reported %d bytes, want %d", got, len(body))
	}

	// From a chroot into the directory, which is how a subdirectory mount
	// reaches the same file: the report names the path from the SHARE root.
	sub, err := share.fs.Chroot("pkg")
	if err != nil {
		t.Fatal(err)
	}
	f, err = sub.Open("f.go")
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := f.ReadAt(buf, 0); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if got := log.get(exportOf(dir), "/pkg/f.go"); got != int64(len(body))+4 {
		t.Errorf("after a 4-byte ReadAt from a chroot, total is %d, want %d", got, len(body)+4)
	}
}

// With no observer a share costs nothing and reports nothing, which is every
// share without a cache.
func TestNoObserverIsNoCost(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	share := dirShare(t, dir)
	f, err := share.fs.Open("a")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, wrapped := f.(*observedFile); wrapped {
		t.Error("a file was wrapped with nobody to report to")
	}
}
