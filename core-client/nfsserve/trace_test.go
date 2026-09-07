package nfsserve

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5"
)

func TestTraceThreshold(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{
		{"250ms", 250 * time.Millisecond},
		{"1s", time.Second},
		{" 2s ", 2 * time.Second},
		{"250", 250 * time.Millisecond},
		{"", 0},
		{"0", 0},
		{"0s", 0},
		// Refused rather than standing for a default nobody asked for.
		{"yes", 0},
		// Below the report's own resolution, where every call clears the
		// threshold and every answer is took=0s.
		{"1ns", 0},
		{"500us", 0},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("REMOTE_DOCKER_NFS_TRACE", tc.value)
			if got := traceThreshold(nil); got != tc.want {
				t.Errorf("traceThreshold() = %v with %q, want %v", got, tc.value, tc.want)
			}
		})
	}

	t.Run("unset", func(t *testing.T) {
		unsetTrace(t)
		if got := traceThreshold(nil); got != 0 {
			t.Errorf("traceThreshold() = %v unset, want 0", got)
		}
	})
}

// unsetTrace removes the switch for one test. Through t.Setenv so it is
// restored for the rest of the package, which os.Unsetenv alone would not do.
func unsetTrace(t *testing.T) {
	t.Helper()
	t.Setenv("REMOTE_DOCKER_NFS_TRACE", "1s")
	if err := os.Unsetenv("REMOTE_DOCKER_NFS_TRACE"); err != nil {
		t.Fatal(err)
	}
}

// tracedLayer reports whether a traceFS sits anywhere in a share's filesystem,
// which is what has to be absent when nobody asked for it: the wrapper costs a
// time.Now and a locked map update on EVERY call, and a single-file share
// hides it one layer down.
func tracedLayer(fs billy.Filesystem) bool {
	for {
		switch v := fs.(type) {
		case *traceFS:
			return true
		case *singleFileFS:
			fs = v.Filesystem
		case *attrFS:
			fs = v.Filesystem
		default:
			return false
		}
	}
}

func TestShareFSIsTracedOnlyWhenAsked(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "only.conf"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Both share shapes, because a single-file share puts the wrapper under
	// singleFileFS, where a check on the outermost type cannot see it.
	for _, file := range []string{"", "only.conf"} {
		unsetTrace(t)
		if tracedLayer(NewRegistry(DefaultAttrs).shareFS(dir, file)) {
			t.Errorf("the share is traced with the switch unset (file %q)", file)
		}

		t.Setenv("REMOTE_DOCKER_NFS_TRACE", "1s")
		if !tracedLayer(NewRegistry(DefaultAttrs).shareFS(dir, file)) {
			t.Errorf("the share is not traced with the switch set (file %q)", file)
		}
	}
}

// The tracer reports and changes nothing: every call returns what the layer
// below returned, error value included. Upstream matches on syscall.EINVAL
// (names.go) and on os.ErrNotExist, so an error the wrapper reshaped would
// change what the server answers.
func TestTracedCallsReturnWhatAnUntracedOneDoes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	plain := NewRegistry(DefaultAttrs).shareFS(dir, "")
	traced := withTrace(NewRegistry(DefaultAttrs).shareFS(dir, ""), dir, nil, time.Hour)

	for _, tc := range []struct {
		name string
		call func(billy.Filesystem) error
	}{
		{"Stat of a file", func(fs billy.Filesystem) error { _, err := fs.Stat("hello.txt"); return err }},
		{"Stat of nothing", func(fs billy.Filesystem) error { _, err := fs.Stat("absent"); return err }},
		{"Lstat of nothing", func(fs billy.Filesystem) error { _, err := fs.Lstat("absent"); return err }},
		{"Open of nothing", func(fs billy.Filesystem) error { _, err := fs.Open("absent"); return err }},
		{"ReadDir of nothing", func(fs billy.Filesystem) error { _, err := fs.ReadDir("absent"); return err }},
		{"Remove of nothing", func(fs billy.Filesystem) error { return fs.Remove("absent") }},
		{"Rename of nothing", func(fs billy.Filesystem) error { return fs.Rename("absent", "elsewhere") }},
		{"Remove that leaves the share", func(fs billy.Filesystem) error { return fs.Remove("..") }},
		// The spelling rule, whose refusal upstream matches on by identity.
		{"create of an unspellable name", func(fs billy.Filesystem) error {
			_, err := fs.OpenFile("quote\"", os.O_CREATE|os.O_WRONLY, 0o644)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want, got := tc.call(plain), tc.call(traced)
			switch {
			case want == nil && got == nil:
			case want == nil || got == nil:
				t.Fatalf("traced = %v, untraced = %v", got, want)
			case got.Error() != want.Error():
				t.Errorf("traced error %q, untraced %q", got, want)
			}
			for _, sentinel := range []error{os.ErrNotExist, os.ErrPermission, syscall.EINVAL} {
				if errors.Is(want, sentinel) != errors.Is(got, sentinel) {
					t.Errorf("errors.Is(err, %v) = %v traced, %v untraced", sentinel,
						errors.Is(got, sentinel), errors.Is(want, sentinel))
				}
			}
		})
	}
}

// The same over the wire: a traced share serves and accepts exactly what an
// untraced one does.
func TestTracedShareServesTheSameFiles(t *testing.T) {
	t.Setenv("REMOTE_DOCKER_NFS_TRACE", "1ms")

	dir := t.TempDir()
	const content = "read through the tracer\n"
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	target := mountCWD(t, dir)

	f, err := target.Open("hello.txt")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, err := io.ReadAll(f)
	f.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != content {
		t.Errorf("read %q, want %q", got, content)
	}

	const written = "written through the tracer"
	out, err := target.OpenFile("out.txt", 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := out.Write([]byte(written)); err != nil {
		t.Fatalf("write: %v", err)
	}
	out.Close()

	onDisk, err := os.ReadFile(filepath.Join(dir, "out.txt"))
	if err != nil {
		t.Fatalf("reading what was written: %v", err)
	}
	if string(onDisk) != written {
		t.Errorf("on disk %q, want %q", onDisk, written)
	}

	if err := target.Remove("out.txt"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "out.txt")); !os.IsNotExist(err) {
		t.Errorf("the file is still there after REMOVE: %v", err)
	}
}

// go-nfs answers READ with Open, ReadAt and Close (nfs_onread.go), never Read,
// so timing Read alone leaves every byte a share serves untimed and reports a
// read count of zero. That is worse than no tracer: the reader concludes the
// filesystem is not where the time goes.
func TestTraceTimesTheServersReadPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	fs := withTrace(NewRegistry(DefaultAttrs).shareFS(dir, ""), dir, nil, time.Hour).(*traceFS)

	f, err := fs.Open("hello.txt")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := f.ReadAt(make([]byte, 4), 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()
	for _, op := range []string{"Open", "ReadAt", "Close"} {
		if fs.count[op] != 1 {
			t.Errorf("counted %d %s, want 1: %v", fs.count[op], op, fs.count)
		}
	}
}

// What the report has to carry to be worth reading: the operation, the path,
// and the running count that tells one outlier from a share doing this all
// the time. observe is called directly because Windows's monotonic clock is
// coarse enough to time a real Stat at zero.
func TestTraceReportsASlowCall(t *testing.T) {
	var buf bytes.Buffer
	fs := withTrace(NewRegistry(DefaultAttrs).shareFS(t.TempDir(), ""), "/cwd", slog.New(slog.NewTextHandler(&buf, nil)), time.Second).(*traceFS)
	fs.observe("Stat", "hello.txt", time.Now().Add(-2*time.Second))

	line := buf.String()
	for _, want := range []string{"op=Stat", "path=hello.txt", "share=/cwd", "calls=1", "took=2", "mean=2"} {
		if !strings.Contains(line, want) {
			t.Errorf("the report does not carry %q: %s", want, line)
		}
	}
}

// A call under the threshold says nothing, which is what makes leaving the
// switch on bearable, and it is still counted, so the next slow one reports a
// mean over everything rather than over the outliers.
func TestTraceCountsQuietly(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	fs := withTrace(NewRegistry(DefaultAttrs).shareFS(dir, ""), dir, slog.New(slog.NewTextHandler(&buf, nil)), time.Hour).(*traceFS)
	if _, err := fs.Stat("hello.txt"); err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if _, err := fs.ReadDir("."); err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	if buf.Len() != 0 {
		t.Errorf("a fast call was reported: %s", buf.String())
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.count["Stat"] != 1 || fs.count["ReadDir"] != 1 {
		t.Errorf("counted %v, want one Stat and one ReadDir", fs.count)
	}
}

// The summary is keyed by operation, so serving a large tree cannot grow it.
// Keyed by path it would hold one entry per file for the life of the session,
// on a filesystem the user chose precisely because it is large.
func TestTraceSummaryDoesNotGrowWithPaths(t *testing.T) {
	dir := t.TempDir()
	fs := withTrace(NewRegistry(DefaultAttrs).shareFS(dir, ""), dir, nil, time.Hour).(*traceFS)

	// Concurrently, because go-nfs dispatches requests in parallel and the
	// summary is shared between them.
	var wg sync.WaitGroup
	for i := range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = fs.Stat(filepath.Join("a", "b", strconv.Itoa(i)))
		}()
	}
	wg.Wait()

	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.count) != 1 || fs.count["Stat"] != 200 {
		t.Errorf("summary is %v, want one Stat entry counting 200", fs.count)
	}
}
