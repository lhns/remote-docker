package nfsserve

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-billy/v5"
)

// Keep a file open across the requests that write it.
//
// NFS has no open file, so go-nfs opens, seeks, writes and CLOSES on every
// WRITE request: a 185MB file written at wsize=1048576 is opened and closed
// 180 times. On Windows an open of a file being written costs between 1.3 and
// 12.4 seconds, against 0.2ms for the same file once nothing is writing it: a
// scanner re-reads what each close finished and the next open waits behind it,
// so the cost grows with the file and is paid per megabyte. It is wasted work
// with or without a scanner, so the file stays open for a short while after a
// request lets go of it and the next request reuses it.
//
// The descriptor is SHARED, so a request may not use the file's own offset:
// two writes interleaving their Seek and Write would land wherever the other
// left it. Each caller keeps its own offset and the pair is done under one
// lock, which is what makes sharing safe.
type fdCacheFS struct {
	billy.Filesystem

	idle time.Duration
	max  int

	mu   sync.Mutex
	open map[string]*cachedFD
}

// fdCacheMax bounds how many descriptors are held at once.
const fdCacheMax = 64

// fdCacheIdle is how long a file stays open after the last request that used
// it. REMOTE_DOCKER_NFS_FDCACHE tunes it, and 0 turns the cache off.
//
// Short, because the descriptor is held on the user's own machine: two seconds
// covers the gap between the writes of one file, which arrive milliseconds
// apart, and leaves nothing held that anyone would notice.
func fdCacheIdle() time.Duration {
	v, ok := os.LookupEnv("REMOTE_DOCKER_NFS_FDCACHE")
	if !ok || v == "" {
		return 2 * time.Second
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 2 * time.Second
	}
	return d
}

func withFDCache(fs billy.Filesystem, idle time.Duration, max int) billy.Filesystem {
	return &fdCacheFS{Filesystem: fs, idle: idle, max: max, open: map[string]*cachedFD{}}
}

// cachedFD is one open file and the requests currently holding it.
type cachedFD struct {
	mu   sync.Mutex // serialises seek+write pairs on the shared descriptor
	file *os.File

	owner *fdCacheFS
	path  string

	refs  int
	timer *time.Timer
}

// cacheable is the shape go-nfs's read and write paths open a file with. A
// create, a truncate or an append is passed through untouched: those change
// the file's identity or its length, and sharing one across requests would
// make the order they arrive in matter.
func cacheable(flag int) bool {
	const forbidden = os.O_CREATE | os.O_TRUNC | os.O_EXCL | os.O_APPEND
	return flag&forbidden == 0
}

func (c *fdCacheFS) OpenFile(name string, flag int, perm os.FileMode) (billy.File, error) {
	if !cacheable(flag) {
		// The identity or the length is about to change, so anything held for
		// this name is stale and must not outlive the call.
		c.evict(name)
		return c.Filesystem.OpenFile(name, flag, perm)
	}

	c.mu.Lock()
	if e, ok := c.open[name]; ok {
		e.refs++
		if e.timer != nil {
			e.timer.Stop()
			e.timer = nil
		}
		c.mu.Unlock()
		return &sharedFile{fd: e}, nil
	}
	if len(c.open) >= c.max {
		// Uncached rather than unbounded. billy's own descriptor is right for
		// this one, because nothing holds it past the request.
		c.mu.Unlock()
		return c.Filesystem.OpenFile(name, flag, perm)
	}
	c.mu.Unlock()

	// Opened through billy first, which is what resolves the path inside the
	// share, and then reopened at the path it resolved to: a descriptor held
	// across requests has to allow a delete, which billy's open does not on
	// Windows. Containment is billy's either way.
	resolved, err := c.Filesystem.OpenFile(name, flag, perm)
	if err != nil {
		return resolved, err
	}
	path := resolved.Name()
	_ = resolved.Close()

	f, err := openShared(path)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	if e, ok := c.open[name]; ok {
		// Another request opened it while this one was in the filesystem. Keep
		// the first, so there is only ever one descriptor per path to order
		// writes against.
		e.refs++
		c.mu.Unlock()
		_ = f.Close()
		return &sharedFile{fd: e}, nil
	}
	e := &cachedFD{file: f, owner: c, path: name, refs: 1}
	c.open[name] = e
	c.mu.Unlock()

	return &sharedFile{fd: e}, nil
}

// release is called when a request is done with the file. The descriptor stays
// open for idle, because the next WRITE of the same file is normally already
// on its way.
func (c *fdCacheFS) release(e *cachedFD) {
	c.mu.Lock()

	e.refs--
	if e.refs > 0 {
		c.mu.Unlock()
		return
	}

	// Evicted while it was in use: nothing can reach it again, and the idle
	// timer looks entries up by name, so it would find whatever took this
	// name next or nothing at all. Either way this descriptor would never be
	// closed, and on Windows it holds the file open. Close it here.
	if c.open[e.path] != e {
		c.mu.Unlock()
		e.close()
		return
	}

	if e.timer != nil {
		e.timer.Stop()
	}
	e.timer = time.AfterFunc(c.idle, func() { c.evictEntry(e) })
	c.mu.Unlock()
}

// close shuts the descriptor once, under the lock that orders writes on it.
func (e *cachedFD) close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	_ = e.file.Close()
}

// evict drops whatever is held for name. A file still in use is dropped from
// the map and closed by its last release instead, so no later request finds a
// descriptor that is about to stop meaning this name.
func (c *fdCacheFS) evict(name string) {
	c.mu.Lock()
	e, ok := c.open[name]
	c.mu.Unlock()
	if ok {
		c.evictEntry(e)
	}
}

// evictEntry is evict for an entry already in hand, which is what the idle
// timer has. It removes only THIS entry: by the time a timer fires the name
// may belong to a different descriptor, and closing that one would take a
// live file away from whatever is writing it.
func (c *fdCacheFS) evictEntry(e *cachedFD) {
	c.mu.Lock()
	if c.open[e.path] == e {
		delete(c.open, e.path)
	}
	if e.timer != nil {
		e.timer.Stop()
		e.timer = nil
	}
	inUse := e.refs > 0
	c.mu.Unlock()

	if !inUse {
		e.close()
	}
}

// A name that stops existing, or stops meaning this file, must not be served
// from a descriptor opened before it changed.
//
// By PREFIX, not by name: renaming a directory means renaming everything under
// it, and Windows refuses to rename a directory that holds an open file even
// when the file itself was opened to allow it. That presents as a RENAME
// answering NFS3ERR_ACCES because a write to a file two levels down still had
// a descriptor cached.
func (c *fdCacheFS) Remove(name string) error {
	c.evictTree(name)
	return c.Filesystem.Remove(name)
}

func (c *fdCacheFS) Rename(from, to string) error {
	c.evictTree(from)
	c.evictTree(to)
	return c.Filesystem.Rename(from, to)
}

// evictTree drops name and everything below it.
func (c *fdCacheFS) evictTree(name string) {
	c.mu.Lock()
	held := make([]string, 0, len(c.open))
	for path := range c.open {
		if path == name || under(name, path) {
			held = append(held, path)
		}
	}
	c.mu.Unlock()

	for _, path := range held {
		c.evict(path)
	}
}

// under reports whether path is inside dir, comparing the way the names
// arrive: go-nfs joins handle components with the filesystem's separator, and
// a share is asked about both spellings on Windows.
func under(dir, path string) bool {
	dir = filepath.Clean(filepath.FromSlash(dir))
	path = filepath.Clean(filepath.FromSlash(path))
	if dir == "." || dir == string(filepath.Separator) {
		return true
	}
	return strings.HasPrefix(path, dir+string(filepath.Separator))
}

// sharedFile is one request's view of a shared descriptor: its own offset, and
// no ownership of the file underneath.
type sharedFile struct {
	fd     *cachedFD
	offset int64
	closed bool
}

func (s *sharedFile) Name() string { return s.fd.file.Name() }

func (s *sharedFile) Write(p []byte) (int, error) {
	s.fd.mu.Lock()
	defer s.fd.mu.Unlock()

	if _, err := s.fd.file.Seek(s.offset, 0); err != nil {
		return 0, err
	}
	n, err := s.fd.file.Write(p)
	s.offset += int64(n)
	return n, err
}

func (s *sharedFile) Read(p []byte) (int, error) {
	s.fd.mu.Lock()
	defer s.fd.mu.Unlock()

	if _, err := s.fd.file.Seek(s.offset, 0); err != nil {
		return 0, err
	}
	n, err := s.fd.file.Read(p)
	s.offset += int64(n)
	return n, err
}

func (s *sharedFile) ReadAt(p []byte, off int64) (int, error) {
	s.fd.mu.Lock()
	defer s.fd.mu.Unlock()
	return s.fd.file.ReadAt(p, off)
}

func (s *sharedFile) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case 0:
		s.offset = offset
	case 1:
		s.offset += offset
	default:
		// From the end, which needs the file, and therefore the lock.
		s.fd.mu.Lock()
		defer s.fd.mu.Unlock()
		n, err := s.fd.file.Seek(offset, whence)
		if err == nil {
			s.offset = n
		}
		return n, err
	}
	return s.offset, nil
}

func (s *sharedFile) Truncate(size int64) error {
	s.fd.mu.Lock()
	defer s.fd.mu.Unlock()
	return s.fd.file.Truncate(size)
}

func (s *sharedFile) Lock() error   { return errNoServerLocks }
func (s *sharedFile) Unlock() error { return errNoServerLocks }

// Close gives the descriptor back rather than closing it, which is the whole
// point. Closing twice must not release twice.
func (s *sharedFile) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	s.fd.owner.release(s.fd)
	return nil
}

// errNoServerLocks is what a lock through this layer answers.
//
// Shares are mounted nolock and go-nfs implements no NLM, so the server is
// never asked to lock: a container's flock and fcntl locks are its own
// kernel's. Answering an error rather than nil means that if the server ever
// IS asked, it fails where it can be seen instead of reporting a lock nobody
// took.
var errNoServerLocks = errors.New("nfsserve: this share does not lock on the server; it is mounted nolock")
