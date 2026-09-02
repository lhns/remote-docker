package nfsserve

import (
	"fmt"
	"hash/fnv"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-git/go-billy/v5"
	nfsfile "github.com/willscott/go-nfs/file"
)

// Attrs are the ownership and permission bits reported to the workspace for
// every file in a share.
//
// They are synthesised rather than read from the local filesystem, and that is
// the point. Windows has no uid or gid to report, which is why rclone, whose
// --uid/--gid/--umask are unsupported there, made every file appear as uid
// 1000 and made chown inside a container fail. Serving the filesystem
// ourselves means reporting the uid the workspace account actually has.
type Attrs struct {
	UID uint32
	GID uint32

	// FileMode and DirMode are permission bits only; the type bits of the
	// real file are preserved.
	FileMode os.FileMode
	DirMode  os.FileMode

	// AlwaysExecutable reports every file as executable.
	//
	// Needed where the local filesystem has no execute bit to preserve, which
	// means Windows. Without it nothing on the share can be run, not a
	// committed binary, not ./scripts/build.sh, not a Makefile's helper --
	// because a synthesised 0644 has no execute bit and the container gets
	// "permission denied". Mounting a FAT or NTFS volume on Linux makes the
	// same trade for the same reason.
	//
	// Where the local filesystem does have an execute bit, leave this off:
	// the real bits are preserved instead, which is strictly better.
	AlwaysExecutable bool
}

// executableBits are the permissions granted when a file is executable.
const executableBits = 0o111

// DefaultAttrs is what a share reports when the account's uid is unknown.
//
// Wide bits rather than a guessed owner: a file readable and writable by
// anyone is usable by whatever uid the container happens to run as, where a
// wrong owner with tight bits is not usable by anyone.
var DefaultAttrs = Attrs{
	UID:      0,
	GID:      0,
	FileMode: 0o666,
	DirMode:  0o777,
}

// attrFS reports Attrs for everything beneath a real filesystem.
type attrFS struct {
	billy.Filesystem
	attrs Attrs

	// export is which share this filesystem serves, so a handle can name it
	// (ADR 0033). Carried here because go-nfs hands the handler a filesystem
	// and nothing else, and because the POINTER cannot be the identity:
	// SetAttrs rebuilds every share's filesystem on each connect.
	export string

	// isRoot distinguishes the share itself from a Chroot into a subdirectory
	// of it. Both carry the same export, and only the first may be given the
	// derived root handle -- handing it to a subdirectory mount would resolve
	// every later lookup against the share root instead, which is a mount
	// silently serving the wrong directory.
	isRoot bool
}

// withAttrs wraps fs so every FileInfo it returns carries the given ownership
// and permissions, and so it can say which share it is.
func withAttrs(inner billy.Filesystem, attrs Attrs, export string) billy.Filesystem {
	return &attrFS{Filesystem: inner, attrs: attrs, export: export, isRoot: true}
}

// exportRootOf reports the share a filesystem is the ROOT of, and "" for a
// subdirectory of one or for anything this package did not wrap.
func exportRootOf(fs billy.Filesystem) string {
	if a, ok := fs.(*attrFS); ok && a.isRoot {
		return a.export
	}
	return ""
}

func (a *attrFS) Stat(name string) (os.FileInfo, error) {
	fi, err := a.Filesystem.Stat(name)
	if err != nil {
		return nil, err
	}
	return a.wrap(fi, name), nil
}

func (a *attrFS) Lstat(name string) (os.FileInfo, error) {
	fi, err := a.Filesystem.Lstat(name)
	if err != nil {
		return nil, err
	}
	return a.wrap(fi, name), nil
}

func (a *attrFS) ReadDir(dir string) ([]os.FileInfo, error) {
	entries, err := a.Filesystem.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]os.FileInfo, len(entries))
	for i, fi := range entries {
		out[i] = a.wrap(fi, path.Join(dir, fi.Name()))
	}
	return out, nil
}

// Chroot must re-wrap, or descending into a subdirectory would quietly drop
// back to the underlying filesystem's real attributes.
func (a *attrFS) Chroot(p string) (billy.Filesystem, error) {
	inner, err := a.Filesystem.Chroot(p)
	if err != nil {
		return nil, err
	}
	// The same share and the same attributes, but NOT the share's root: this
	// is a directory inside it, and a mount of it resolves against itself.
	return &attrFS{Filesystem: inner, attrs: a.attrs, export: a.export}, nil
}

func (a *attrFS) wrap(fi os.FileInfo, fullPath string) os.FileInfo {
	return &attrInfo{FileInfo: fi, attrs: a.attrs, path: fullPath}
}

// attrInfo overrides the ownership and permission bits of a real FileInfo.
type attrInfo struct {
	os.FileInfo
	attrs Attrs
	path  string
}

// Mode keeps the type bits (directory, symlink, device) and replaces only
// the permissions. Reporting a directory as a regular file would break
// traversal outright.
func (i *attrInfo) Mode() fs.FileMode {
	mode := i.FileInfo.Mode()
	if mode.IsDir() {
		return (mode &^ fs.ModePerm) | (i.attrs.DirMode & fs.ModePerm)
	}

	perm := i.attrs.FileMode & fs.ModePerm

	// Executability is the one permission taken from the real file rather than
	// synthesised, because getting it wrong means a script on the share cannot
	// be run at all. Where the local filesystem cannot express it, the share
	// says yes. See Attrs.AlwaysExecutable.
	if i.attrs.AlwaysExecutable || mode&executableBits != 0 {
		perm |= executableBits
	}
	return (mode &^ fs.ModePerm) | perm
}

// Sys is how the attributes actually reach the wire: go-nfs checks Sys() for
// its own file.FileInfo before falling back to platform-specific decoding,
// and that fallback returns nothing at all on Windows.
func (i *attrInfo) Sys() any {
	return &nfsfile.FileInfo{
		Nlink:  1,
		UID:    i.attrs.UID,
		GID:    i.attrs.GID,
		Fileid: fileID(i.path),
	}
}

// fileID gives each path a stable identifier, standing in for an inode number.
//
// NFS clients use it to tell files apart and to detect that two names are the
// same file, so it must be stable for a given path and distinct between paths.
// Windows exposes no inode through os.FileInfo, so it is derived from the path,
// which is what go-nfs itself does when it cannot find a real one.
func fileID(p string) uint64 {
	h := fnv.New64()
	_, _ = h.Write([]byte(p))
	return h.Sum64()
}

// attrChange satisfies billy.Change so the workspace can issue SETATTR.
//
// Returning nil instead would make go-nfs treat the export as read-only, which
// would defeat the entire purpose.
//
// It operates on the real path rather than delegating, because there is nothing
// to delegate TO: go-billy's osfs does not implement billy.Change at all. The
// first version asked it to and accepted a nil, which made every attribute
// write a silent success -- a client was told its chmod landed and the file
// never changed.
type attrChange struct {
	// root is the share's directory on this machine, which share-relative
	// names are resolved against.
	root string
}

// Chmod sets the permissions, which is how a file becomes executable.
//
// Without it a binary built on a share links, is reported executable by the
// synthesised attributes, and cannot be run: the bit was never written.
func (c *attrChange) Chmod(name string, mode os.FileMode) error {
	target, err := c.resolve(name)
	if err != nil {
		return err
	}
	return os.Chmod(target, mode)
}

// Chown and Lchown are accepted and discarded.
//
// Ownership is synthesised (see Attrs), so there is nothing to record: a
// container's `chown -R app:app .` cannot change what we report. Accepting it
// is nonetheless the right behaviour, because refusing makes chown fail,
// which is the exact rclone limitation this package exists to remove, and
// installers and entrypoint scripts that chown their working directory are
// common. The following stat reports the configured uid either way.
func (c *attrChange) Chown(string, int, int) error  { return nil }
func (c *attrChange) Lchown(string, int, int) error { return nil }

// Chtimes is real: timestamps are read from the underlying filesystem, so a
// change here is observable, and build tools depend on mtime.
func (c *attrChange) Chtimes(name string, atime, mtime time.Time) error {
	target, err := c.resolve(name)
	if err != nil {
		return err
	}
	return os.Chtimes(target, atime, mtime)
}

// resolve turns a share-relative name into a path on this machine, refusing
// anything that leaves the share.
//
// Checked on the RESULT, because filepath.Join cleans: "../.." looks like an
// ordinary path afterwards. The name reaches here from the workspace, and the
// workspace is not this machine's to trust with a path.
func (c *attrChange) resolve(name string) (string, error) {
	if c.root == "" {
		return "", fmt.Errorf("nfsserve: no share directory to write attributes in")
	}
	target := filepath.Join(c.root, filepath.FromSlash(name))
	prefix := strings.TrimSuffix(c.root, string(filepath.Separator)) + string(filepath.Separator)
	if target != c.root && !strings.HasPrefix(target, prefix) {
		return "", fmt.Errorf("nfsserve: %q leaves the share", name)
	}
	return target, nil
}
