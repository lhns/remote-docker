package nfsserve

import (
	"io/fs"
	"os"
	"time"

	"github.com/go-git/go-billy/v5"
	nfs "github.com/willscott/go-nfs"
)

// A read-only share (ADR 0058) refuses every mutation here, on this machine's
// server, because `ro` on the container's mount binds only that container.
//
// It is a wrapper and not a capability flag. go-nfs answers a filesystem
// without billy.WriteCapability by refusing SETATTR outright and by stripping
// write from ACCESS, and the agent's poker needs both to work: it sets a file's
// mtime to the value it already has, over NFS, and the kernel raises the
// inotify event only when that SETATTR succeeds. So the capability stays, and
// what the handlers reach is refused one layer down.

// errReadOnly is what a refused mutation is. go-nfs maps a permission error to
// NFS3ERR_ACCES on every path that reaches these methods; NFS3ERR_ROFS is
// answered only by the capability check this design must not trigger.
func errReadOnly(op, name string) error {
	return &fs.PathError{Op: "nfsserve: " + op + " on a read-only share", Path: name, Err: os.ErrPermission}
}

// writing is the open flags that can change a file.
const writing = os.O_WRONLY | os.O_RDWR | os.O_APPEND | os.O_CREATE | os.O_TRUNC

// readOnlyFS refuses what changes the share and passes through what reads it.
// It sits directly under attrFS, so a Chroot into a subdirectory stays wrapped.
type readOnlyFS struct {
	billy.Filesystem
}

// withReadOnly wraps inner for a read-only share.
func withReadOnly(inner billy.Filesystem) billy.Filesystem { return &readOnlyFS{Filesystem: inner} }

func (r *readOnlyFS) Create(name string) (billy.File, error) {
	return nil, errReadOnly("create", name)
}

func (r *readOnlyFS) OpenFile(name string, flag int, perm os.FileMode) (billy.File, error) {
	if flag&writing != 0 {
		return nil, errReadOnly("open for writing", name)
	}
	return r.Filesystem.OpenFile(name, flag, perm)
}

func (r *readOnlyFS) Rename(from, _ string) error { return errReadOnly("rename", from) }
func (r *readOnlyFS) Remove(name string) error    { return errReadOnly("remove", name) }

func (r *readOnlyFS) MkdirAll(name string, _ os.FileMode) error {
	return errReadOnly("mkdir", name)
}

func (r *readOnlyFS) Symlink(_, link string) error { return errReadOnly("symlink", link) }

func (r *readOnlyFS) TempFile(dir, _ string) (billy.File, error) {
	return nil, errReadOnly("create a temporary file", dir)
}

func (r *readOnlyFS) Chroot(p string) (billy.Filesystem, error) {
	inner, err := r.Filesystem.Chroot(p)
	if err != nil {
		return nil, err
	}
	return &readOnlyFS{Filesystem: inner}, nil
}

// readOnlyOf reports whether a share's filesystem, as go-nfs hands it back
// wrapped in attributes, refuses writes.
func readOnlyOf(f billy.Filesystem) bool {
	for {
		switch v := f.(type) {
		case *readOnlyFS:
			return true
		case *attrFS:
			f = v.Filesystem
		default:
			return false
		}
	}
}

// readOnlyChange is the same refusal for the operations that do not go through
// the filesystem: go-nfs calls a billy.Change with paths, so attrChange would
// write the real directory beside everything above.
//
// The one thing it lets through is an identity time-set (see Chtimes).
type readOnlyChange struct {
	fs billy.Filesystem
}

var _ nfs.UnixChange = (*readOnlyChange)(nil)

func (c *readOnlyChange) Chmod(name string, _ os.FileMode) error {
	return errReadOnly("chmod", name)
}
func (c *readOnlyChange) Chown(name string, _, _ int) error  { return errReadOnly("chown", name) }
func (c *readOnlyChange) Lchown(name string, _, _ int) error { return errReadOnly("chown", name) }

func (c *readOnlyChange) Link(_, link string) error { return errReadOnly("link", link) }
func (c *readOnlyChange) Mknod(name string, _, _, _ uint32) error {
	return errReadOnly("mknod", name)
}
func (c *readOnlyChange) Mkfifo(name string, _ uint32) error { return errReadOnly("mkfifo", name) }
func (c *readOnlyChange) Socket(name string) error           { return errReadOnly("socket", name) }

// Chtimes accepts a time-set that changes nothing and refuses every other.
//
// The share reports a file's atime, mtime and ctime as one value, its
// modification time, so a set is an identity exactly when both times it carries
// equal that. go-nfs fills in the one a client left out from the current value,
// so the agent's poker (an mtime set to the file's own, atime omitted) arrives
// as an identity, succeeds, and the kernel raises its inotify event. A set to
// any other time is refused, `touch` included. Like the read-write share's
// Chtimes, an accepted one is not applied.
func (c *readOnlyChange) Chtimes(name string, atime, mtime time.Time) error {
	info, err := c.fs.Lstat(name)
	if err != nil {
		return err
	}
	current := info.ModTime()
	same := func(t time.Time) bool {
		return t.Unix() == current.Unix() && t.Nanosecond() == current.Nanosecond()
	}
	if !same(atime) || !same(mtime) {
		return errReadOnly("set times", name)
	}
	return nil
}
