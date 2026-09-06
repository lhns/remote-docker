package nfsserve

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
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
	if _, err := r.RegisterCWD(dir); err != nil {
		t.Fatal(err)
	}
	share, _, ok := r.Lookup("/cwd")
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
	if got := log.get("/cwd", "/pkg/f.go"); got != int64(len(body)) {
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
	if got := log.get("/cwd", "/pkg/f.go"); got != int64(len(body))+4 {
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
	share := cwdShare(t, dir)
	f, err := share.fs.Open("a")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, wrapped := f.(*observedFile); wrapped {
		t.Error("a file was wrapped with nobody to report to")
	}
}

// SetAttrs rebuilds every share's filesystem on every connect. For a
// single-file share it rebuilt it as a directory rooted at the FILE, dropping
// the wrapper that hides the file's siblings and rooting osfs at something
// that is not a directory.
func TestSetAttrsKeepsASingleFileShareSingle(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"app.conf", "secret.conf"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	r := NewRegistry(DefaultAttrs)
	if _, err := r.Register(filepath.Join(dir, "app.conf")); err != nil {
		t.Fatal(err)
	}
	var share *Share
	for _, s := range r.Shares() {
		share = s
	}
	if share == nil || share.File != "app.conf" {
		t.Fatalf("share = %+v, want a single-file share of app.conf", share)
	}

	check := func(when string) {
		t.Helper()
		if _, err := share.fs.Stat("app.conf"); err != nil {
			t.Errorf("%s: the file itself is not reachable: %v", when, err)
		}
		if _, err := share.fs.Stat("secret.conf"); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s: the sibling is reachable (err=%v); the share stopped being a single file", when, err)
		}
	}
	check("before SetAttrs")
	r.SetAttrs(Attrs{UID: 1000, GID: 1000, FileMode: 0o644, DirMode: 0o755})
	check("after SetAttrs")
}
