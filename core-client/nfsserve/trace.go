package nfsserve

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-billy/v5"

	"github.com/lhns/remote-docker/core/logx"
)

// Timing for the filesystem calls a request makes, because nothing else here
// measures one: go-nfs reports errors and everything below Warn is dropped
// (logging.go), so a share whose writes take seconds each says nothing at any
// level and the only evidence is on the workspace, in the NFS client's own
// counters.
//
// Off unless REMOTE_DOCKER_NFS_TRACE names a threshold, and not installed at
// all when it does not (shareFS): a time.Now and a locked map update on every
// filesystem call is not something a share should pay for unasked.
type traceFS struct {
	billy.Filesystem

	log   *slog.Logger
	slow  time.Duration
	share string

	// Keyed by operation name, of which there is a fixed handful, so the
	// summary cannot grow with the number of files or requests.
	mu    sync.Mutex
	count map[string]int
	total map[string]time.Duration
}

// traceThreshold reads the switch. Zero means tracing is off.
//
// A value that is neither a duration nor a number is refused rather than
// standing for a default: the variable is set by somebody chasing a symptom,
// and tracing at a threshold they did not ask for is worse than being told the
// value was not understood.
func traceThreshold(log *slog.Logger) time.Duration {
	v, ok := os.LookupEnv("REMOTE_DOCKER_NFS_TRACE")
	if !ok {
		return 0
	}
	v = strings.TrimSpace(v)
	if v == "" || v == "0" {
		return 0
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	if ms, err := strconv.Atoi(v); err == nil {
		return time.Duration(ms) * time.Millisecond
	}
	logx.Or(log).Warn("nfs: REMOTE_DOCKER_NFS_TRACE is not a duration, so tracing is off",
		"value", v, "want", "250ms, or bare milliseconds")
	return 0
}

func withTrace(fs billy.Filesystem, share string, log *slog.Logger, slow time.Duration) billy.Filesystem {
	return &traceFS{
		Filesystem: fs,
		log:        logx.Or(log),
		slow:       slow,
		share:      share,
		count:      map[string]int{},
		total:      map[string]time.Duration{},
	}
}

// observe records one call and reports it if it was slow. The running count
// and mean travel with the report, so a run can be read as a whole rather than
// as a list of outliers.
func (t *traceFS) observe(op, name string, start time.Time) {
	d := time.Since(start)

	t.mu.Lock()
	t.count[op]++
	t.total[op] += d
	n, sum := t.count[op], t.total[op]
	t.mu.Unlock()

	if d < t.slow {
		return
	}
	t.log.Warn("nfs: a filesystem call was slow",
		"op", op, "took", d.Round(time.Millisecond), "path", name,
		"share", t.share, "calls", n, "mean", (sum / time.Duration(n)).Round(time.Millisecond))
}

func (t *traceFS) Stat(name string) (os.FileInfo, error) {
	defer t.observe("Stat", name, time.Now())
	return t.Filesystem.Stat(name)
}

func (t *traceFS) Lstat(name string) (os.FileInfo, error) {
	defer t.observe("Lstat", name, time.Now())
	return t.Filesystem.Lstat(name)
}

func (t *traceFS) Open(name string) (billy.File, error) {
	defer t.observe("Open", name, time.Now())
	return t.Filesystem.Open(name)
}

func (t *traceFS) OpenFile(name string, flag int, perm os.FileMode) (billy.File, error) {
	start := time.Now()
	f, err := t.Filesystem.OpenFile(name, flag, perm)
	t.observe("OpenFile", name, start)
	if err != nil {
		return f, err
	}
	return &traceFile{File: f, fs: t, name: name}, nil
}

func (t *traceFS) ReadDir(name string) ([]os.FileInfo, error) {
	defer t.observe("ReadDir", name, time.Now())
	return t.Filesystem.ReadDir(name)
}

func (t *traceFS) Rename(from, to string) error {
	defer t.observe("Rename", from, time.Now())
	return t.Filesystem.Rename(from, to)
}

func (t *traceFS) Remove(name string) error {
	defer t.observe("Remove", name, time.Now())
	return t.Filesystem.Remove(name)
}

// traceFile times what a request does with an open file. The write path spends
// its time here rather than in the open: go-nfs opens, seeks, writes and
// CLOSES on every WRITE request, so a close that blocks blocks the connection.
type traceFile struct {
	billy.File
	fs   *traceFS
	name string
}

func (f *traceFile) Write(p []byte) (int, error) {
	defer f.fs.observe("Write", f.name, time.Now())
	return f.File.Write(p)
}

func (f *traceFile) Read(p []byte) (int, error) {
	defer f.fs.observe("Read", f.name, time.Now())
	return f.File.Read(p)
}

func (f *traceFile) Seek(offset int64, whence int) (int64, error) {
	defer f.fs.observe("Seek", f.name, time.Now())
	return f.File.Seek(offset, whence)
}

func (f *traceFile) Close() error {
	defer f.fs.observe("Close", f.name, time.Now())
	return f.File.Close()
}
