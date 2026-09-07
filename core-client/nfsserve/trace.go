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
// (logging.go), so a share whose calls take seconds each says nothing at any
// level and the only evidence is on the workspace, in the NFS client's own
// counters.
//
// Off unless REMOTE_DOCKER_NFS_TRACE names a threshold, and not installed at
// all when it does not (Registry.shareFS): a time.Now and a locked map update
// on every filesystem call is not something a share should pay for unasked.
//
// It reports and changes nothing else. Every method returns what the layer
// below returned, error value included, because callers above match on
// syscall.EINVAL and on os.ErrNotExist, and the embedding swallows no optional
// interface the layer below satisfies: billy.Filesystem already carries the
// Symlink go-nfs asserts on, and billy.Change is asserted nowhere, because
// mountHandler.Change builds attrChange from Root() rather than from the
// filesystem. Turning tracing on therefore cannot narrow what the server can
// do, which is the way a wrapper like this usually fails.
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

// minThreshold is the smallest threshold accepted, and it is the report's own
// resolution: a report rounds to milliseconds, so anything under one would
// print took=0s while logging every call a share makes. logx serialises every
// record behind one mutex, so that is not merely noisy, it makes each call
// slow enough to clear the threshold that produced it.
const minThreshold = time.Millisecond

// traceThreshold reads the switch. Zero means tracing is off.
//
// A value that is neither a duration nor a number is refused rather than
// standing for a default: the variable is set by somebody chasing a symptom,
// and tracing at a threshold they did not ask for is worse than being told the
// value was not understood.
func traceThreshold(log *slog.Logger) time.Duration {
	v := strings.TrimSpace(os.Getenv("REMOTE_DOCKER_NFS_TRACE"))
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		ms, numErr := strconv.Atoi(v)
		if numErr != nil {
			logx.Or(log).Warn("nfs: REMOTE_DOCKER_NFS_TRACE is not a duration, so tracing is off",
				"value", v, "want", "250ms, or bare milliseconds")
			return 0
		}
		d = time.Duration(ms) * time.Millisecond
	}
	switch {
	case d == 0:
		return 0
	case d < minThreshold:
		logx.Or(log).Warn("nfs: REMOTE_DOCKER_NFS_TRACE is below the report's own resolution, so tracing is off",
			"value", v, "least", minThreshold)
		return 0
	}
	return d
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

// timed wraps an opened file so what the request does with it is timed too,
// and passes a failure through untouched.
func (t *traceFS) timed(name string, f billy.File, err error) (billy.File, error) {
	if err != nil {
		return f, err
	}
	return &traceFile{File: f, fs: t, name: name}, nil
}

// Every method go-nfs's handlers call is here. One that is missing is worse
// than no tracer at all: a blocking call reports nothing and the reader
// concludes the filesystem is not where the time goes.

func (t *traceFS) Stat(name string) (os.FileInfo, error) {
	defer t.observe("Stat", name, time.Now())
	return t.Filesystem.Stat(name)
}

func (t *traceFS) Lstat(name string) (os.FileInfo, error) {
	defer t.observe("Lstat", name, time.Now())
	return t.Filesystem.Lstat(name)
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

func (t *traceFS) MkdirAll(name string, perm os.FileMode) error {
	defer t.observe("MkdirAll", name, time.Now())
	return t.Filesystem.MkdirAll(name, perm)
}

func (t *traceFS) Symlink(target, link string) error {
	defer t.observe("Symlink", link, time.Now())
	return t.Filesystem.Symlink(target, link)
}

func (t *traceFS) Open(name string) (billy.File, error) {
	start := time.Now()
	f, err := t.Filesystem.Open(name)
	t.observe("Open", name, start)
	return t.timed(name, f, err)
}

func (t *traceFS) OpenFile(name string, flag int, perm os.FileMode) (billy.File, error) {
	start := time.Now()
	f, err := t.Filesystem.OpenFile(name, flag, perm)
	t.observe("OpenFile", name, start)
	return t.timed(name, f, err)
}

func (t *traceFS) Create(name string) (billy.File, error) {
	start := time.Now()
	f, err := t.Filesystem.Create(name)
	t.observe("Create", name, start)
	return t.timed(name, f, err)
}

// traceFile times what a request does with an open file, which is where both
// the read and the write path spend themselves: go-nfs answers READ with
// Open, ReadAt and Close, and WRITE with OpenFile, Seek, Write and Close, so a
// close that blocks blocks the connection.
//
// ReadAt rather than Read is the server's read path. Read is here because
// billy.File carries it and a caller that uses it should be timed the same.
type traceFile struct {
	billy.File
	fs   *traceFS
	name string
}

func (f *traceFile) ReadAt(p []byte, off int64) (int, error) {
	defer f.fs.observe("ReadAt", f.name, time.Now())
	return f.File.ReadAt(p, off)
}

func (f *traceFile) Read(p []byte) (int, error) {
	defer f.fs.observe("Read", f.name, time.Now())
	return f.File.Read(p)
}

func (f *traceFile) Write(p []byte) (int, error) {
	defer f.fs.observe("Write", f.name, time.Now())
	return f.File.Write(p)
}

func (f *traceFile) Seek(offset int64, whence int) (int64, error) {
	defer f.fs.observe("Seek", f.name, time.Now())
	return f.File.Seek(offset, whence)
}

func (f *traceFile) Truncate(size int64) error {
	defer f.fs.observe("Truncate", f.name, time.Now())
	return f.File.Truncate(size)
}

func (f *traceFile) Close() error {
	defer f.fs.observe("Close", f.name, time.Now())
	return f.File.Close()
}
