package nfsserve

import (
	"os"
	"path"
	"strings"

	"github.com/go-git/go-billy/v5"
)

// singleFileFS is a directory containing exactly one file.
//
// A bind mount of a single file has no directory to export, and exporting the
// containing directory would hand the workspace every file beside the one that
// was asked for. So the directory is SYNTHESISED: the export root lists one
// name, and every other name in the real directory is not found.
//
// Nothing is created on this machine -- no symlink, no temporary directory, no
// mount -- because the export namespace is ours to compose (ADR 0007). That is
// also what makes this identical on Windows, macOS and Linux.
//
// The container sees a file rather than a directory containing one because the
// mount carries the file's name as a volume subpath (ADR 0039).
type singleFileFS struct {
	billy.Filesystem // the containing directory, bounded by osfs.WithBoundOS

	name string // the one entry, a base name
}

// slashed normalises a client-supplied path: NFS speaks slashes whatever this
// machine's separator is.
func slashed(p string) string { return path.Clean("/" + strings.ReplaceAll(p, "\\", "/")) }

// isRootPath reports whether p names the export root, which is the containing
// directory and must keep behaving like one: the kernel stats it at mount and
// reads it to find the file.
func isRootPath(p string) bool { return slashed(p) == "/" }

// visible reports whether p names the one file this share exports.
func (s *singleFileFS) visible(p string) bool {
	return strings.TrimPrefix(slashed(p), "/") == s.name
}

func (s *singleFileFS) Open(p string) (billy.File, error) {
	if !s.visible(p) {
		return nil, os.ErrNotExist
	}
	return s.Filesystem.Open(p)
}

func (s *singleFileFS) OpenFile(p string, flag int, perm os.FileMode) (billy.File, error) {
	if !s.visible(p) {
		return nil, os.ErrNotExist
	}
	return s.Filesystem.OpenFile(p, flag, perm)
}

func (s *singleFileFS) Create(p string) (billy.File, error) {
	if !s.visible(p) {
		return nil, os.ErrPermission
	}
	return s.Filesystem.Create(p)
}

func (s *singleFileFS) Stat(p string) (os.FileInfo, error) {
	if isRootPath(p) {
		return s.Filesystem.Stat(p)
	}
	if !s.visible(p) {
		return nil, os.ErrNotExist
	}
	return s.Filesystem.Stat(p)
}

func (s *singleFileFS) Lstat(p string) (os.FileInfo, error) {
	if isRootPath(p) {
		return s.Filesystem.Lstat(p)
	}
	if !s.visible(p) {
		return nil, os.ErrNotExist
	}
	return s.Filesystem.Lstat(p)
}

// ReadDir answers for the root only, with the single entry. A share holding one
// file has no subdirectories, so anything else is not found.
func (s *singleFileFS) ReadDir(p string) ([]os.FileInfo, error) {
	if !isRootPath(p) {
		return nil, os.ErrNotExist
	}
	info, err := s.Filesystem.Lstat(s.name)
	if err != nil {
		return nil, err
	}
	return []os.FileInfo{info}, nil
}

// The mutating operations are refused for any other name, so this export cannot
// be used to create, rename or delete siblings in somebody's directory.

func (s *singleFileFS) Rename(from, to string) error {
	if !s.visible(from) || !s.visible(to) {
		return os.ErrPermission
	}
	return s.Filesystem.Rename(from, to)
}

func (s *singleFileFS) Remove(p string) error {
	if !s.visible(p) {
		return os.ErrPermission
	}
	return s.Filesystem.Remove(p)
}

func (s *singleFileFS) Readlink(p string) (string, error) {
	if !s.visible(p) {
		return "", os.ErrNotExist
	}
	return s.Filesystem.Readlink(p)
}

func (s *singleFileFS) MkdirAll(string, os.FileMode) error { return os.ErrPermission }

func (s *singleFileFS) Symlink(string, string) error { return os.ErrPermission }

func (s *singleFileFS) TempFile(string, string) (billy.File, error) { return nil, os.ErrPermission }

// Chroot has nowhere to go: this filesystem is one file deep.
func (s *singleFileFS) Chroot(string) (billy.Filesystem, error) { return nil, os.ErrNotExist }

// describeMode names what a path is, for a refusal that says why rather than
// what it is not.
func describeMode(m os.FileMode) string {
	switch {
	case m&os.ModeSocket != 0:
		return "socket"
	case m&os.ModeDevice != 0:
		return "device"
	case m&os.ModeNamedPipe != 0:
		return "named pipe"
	case m&os.ModeSymlink != 0:
		return "symlink"
	default:
		return "special file"
	}
}
