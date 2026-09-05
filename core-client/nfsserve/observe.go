package nfsserve

import (
	"os"
	"path"
	"path/filepath"

	"github.com/go-git/go-billy/v5"
)

// Reporting what the workspace reads, for a cache that fills ahead of it.
//
// On a share with a cache, every read that arrives here IS a miss: a hit is
// served from the workspace's own disk and never crosses the network. So the
// stream of reads this server answers is exactly the stream of misses, free
// and exact, and it is the only signal a prefetch policy needs (ADR 0045).

// ReadObserver is told how many bytes were read of a path on a share. The
// path is share-relative, leading slash, forward slashes, the spelling
// core/notify and core/cache use, so the same key names a file on every side.
// Called on the server's own path, so it must be cheap and must not block: a
// share with no cache costs one nil check.
type ReadObserver func(export, path string, n int64)

// observedFile counts what is read through it, per READ rather than per open:
// go-nfs opens a file per READ, so an open says nothing about how much was
// wanted.
type observedFile struct {
	billy.File
	report func(n int64)
}

func (f *observedFile) Read(p []byte) (int, error) {
	n, err := f.File.Read(p)
	if n > 0 {
		f.report(int64(n))
	}
	return n, err
}

func (f *observedFile) ReadAt(p []byte, off int64) (int, error) {
	n, err := f.File.ReadAt(p, off)
	if n > 0 {
		f.report(int64(n))
	}
	return n, err
}

// observe wraps a file so its reads are reported against its share path.
func (a *attrFS) observe(name string, file billy.File) billy.File {
	if a.onRead == nil {
		return file
	}
	// The name is relative to THIS filesystem, which may be a chroot into the
	// share; the prefix is where that chroot sits in the share. Spelled the
	// way the rest of the protocol spells a path, whatever the local OS does.
	full := "/" + path.Clean(path.Join(a.prefix, filepath.ToSlash(name)))
	return &observedFile{
		File:   file,
		report: func(n int64) { a.onRead(a.export, full, n) },
	}
}

func (a *attrFS) Open(name string) (billy.File, error) {
	f, err := a.Filesystem.Open(name)
	if err != nil {
		return nil, err
	}
	return a.observe(name, f), nil
}

func (a *attrFS) OpenFile(name string, flag int, perm os.FileMode) (billy.File, error) {
	f, err := a.Filesystem.OpenFile(name, flag, perm)
	if err != nil {
		return nil, err
	}
	return a.observe(name, f), nil
}
