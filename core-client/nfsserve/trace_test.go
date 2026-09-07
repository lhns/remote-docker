package nfsserve

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("REMOTE_DOCKER_NFS_TRACE", tc.value)
			if got := traceThreshold(nil); got != tc.want {
				t.Errorf("traceThreshold() = %v with %q, want %v", got, tc.value, tc.want)
			}
		})
	}

	t.Run("unset", func(t *testing.T) {
		// Through t.Setenv so the variable is restored for the rest of the
		// package, which os.Unsetenv on its own would not do.
		t.Setenv("REMOTE_DOCKER_NFS_TRACE", "1s")
		os.Unsetenv("REMOTE_DOCKER_NFS_TRACE")
		if got := traceThreshold(nil); got != 0 {
			t.Errorf("traceThreshold() = %v unset, want 0", got)
		}
	})
}

// The wrapper costs a time.Now and a locked map update on every call, so it
// must not be there at all unless the switch asked for it.
func TestShareFSIsTracedOnlyWhenAsked(t *testing.T) {
	dir := t.TempDir()

	t.Setenv("REMOTE_DOCKER_NFS_TRACE", "1s")
	os.Unsetenv("REMOTE_DOCKER_NFS_TRACE")
	if _, traced := NewRegistry(DefaultAttrs).shareFS(dir, "").(*traceFS); traced {
		t.Error("the share is traced with the switch unset")
	}

	t.Setenv("REMOTE_DOCKER_NFS_TRACE", "1s")
	if _, traced := NewRegistry(DefaultAttrs).shareFS(dir, "").(*traceFS); !traced {
		t.Error("the share is not traced with the switch set")
	}
}

// A traced share serves exactly what an untraced one does: the tracer reports
// and changes nothing.
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
