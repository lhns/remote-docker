package nfsserve

import (
	"encoding/binary"
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

	// prefix is where this filesystem sits inside the share: "" for the root,
	// and the chrooted directory otherwise, so a read reported from inside a
	// chroot still names the path from the share root.
	prefix string

	// onRead is told what is read, or nil for a share nobody is watching.
	onRead ReadObserver

	// isRoot distinguishes the share itself from a Chroot into a subdirectory
	// of it. Both carry the same export, and only the first may be given the
	// derived root handle -- handing it to a subdirectory mount would resolve
	// every later lookup against the share root instead, which is a mount
	// silently serving the wrong directory.
	isRoot bool
}

// withAttrs wraps fs so every FileInfo it returns carries the given ownership
// and permissions, and so it can say which share it is.
func withAttrs(inner billy.Filesystem, attrs Attrs, export string, onRead ReadObserver) billy.Filesystem {
	return &attrFS{Filesystem: inner, attrs: attrs, export: export, isRoot: true, onRead: onRead}
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
	return &attrFS{
		Filesystem: inner,
		attrs:      a.attrs,
		export:     a.export,
		prefix:     path.Join(a.prefix, filepath.ToSlash(p)),
		onRead:     a.onRead,
	}, nil
}

func (a *attrFS) wrap(fi os.FileInfo, fullPath string) os.FileInfo {
	// Resolved HERE, while the real FileInfo is in hand: attrInfo.Sys replaces
	// it with the NFS one. The real path goes too, because Windows has to open
	// the file to read an identity.
	// go-nfs stats a just-created file by the absolute OS path osfs returns
	// (nfs_oncreate.go), everything else by the share-relative one. The fileid
	// must come from one spelling, or the CREATE reply carries a number no
	// later reply repeats and the client marks the inode stale.
	sharePath := fullPath
	if filepath.IsAbs(fullPath) {
		if rel, err := filepath.Rel(a.Root(), fullPath); err == nil && !leavesRoot(rel) {
			sharePath = filepath.ToSlash(rel)
		}
	}
	return &attrInfo{
		FileInfo: fi,
		attrs:    a.attrs,
		fileid:   fileIDOf(fi, filepath.Join(a.Root(), sharePath), sharePath),
	}
}

// attrInfo overrides the ownership and permission bits of a real FileInfo.
type attrInfo struct {
	os.FileInfo
	attrs  Attrs
	fileid uint64
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
		Fileid: i.fileid,
	}
}

// fileIDOf is the file's identifier on the wire: the real one the platform
// keeps, and a path hash only where there is none.
//
// A fileid is how a client tells whether a handle still names the same file.
// Change it under a live handle and Linux does not re-resolve; it marks the
// inode stale and every open descriptor on it fails:
//
//	NFS: server error: fileid changed
//	fsid 0:54: expected fileid 0x1548a05f1dee82e0, got 0xd8adad186b880beb
//
// A path hash cannot hold that, because one file has several spellings -- what
// a lookup joined, what a listing joined, what a chroot made relative. It was
// the Windows fallback applied everywhere, and it killed builds on a share:
// `ld` sizes the output it has written, and ftruncate on a stale descriptor is
// what the kernel cannot retry (ADR 0033).
//
// Every platform here has a real identity. Unix has the inode; Windows has
// NTFS's File Reference Number, one GetFileInformationByHandle away.
func fileIDOf(fi os.FileInfo, osPath, sharePath string) uint64 {
	dev, ino, ok := inodeOf(fi, osPath)
	if !ok {
		return fileID(sharePath)
	}
	// Mixed rather than concatenated: both halves are 64 bits and the wire
	// field is 64. The client only compares these for equality, so spreading
	// the loss beats discarding the top of either.
	h := fnv.New64()
	var buf [16]byte
	binary.LittleEndian.PutUint64(buf[:8], dev)
	binary.LittleEndian.PutUint64(buf[8:], ino)
	_, _ = h.Write(buf[:])
	return h.Sum64()
}

// fileID is the fallback: a path hash, for platforms with no inode to report.
func fileID(p string) uint64 {
	h := fnv.New64()
	_, _ = h.Write([]byte(p))
	return h.Sum64()
}

// attrChange satisfies billy.Change so the workspace can issue SETATTR.
// Returning nil instead makes go-nfs treat the export as read-only.
//
// It works on the real path because there is nothing to delegate to: go-billy's
// osfs implements no billy.Change. Asking it to, and accepting the nil, made
// every attribute write a silent success.
type attrChange struct {
	// root is the share's directory on this machine, which share-relative
	// names are resolved against.
	root string
}

// Chmod sets the permissions, which is how a file becomes executable. Without
// it a binary built on a share links and cannot be run.
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

// Chtimes is accepted and NOT applied, deliberately.
//
// The agent replays changes by touching files through this export (ADR 0016).
// Apply them and this machine's watcher sees the touch, reports it, and the
// agent replays it again: one edit became 3063 events in integration.sh
// section 11 when this was real. Breaking the loop needs the watcher to know
// which changes this server caused, and it has no such mechanism.
func (c *attrChange) Chtimes(string, time.Time, time.Time) error { return nil }

// resolve turns a share-relative name into a path on this machine.
//
// Checked on the RESULT: filepath.Join cleans, so "../.." looks ordinary
// afterwards, and the name came from the workspace. The root is cleaned too:
// it arrives spelled as the bind was written, forward slashes on Windows,
// and Join returns the OS spelling.
func (c *attrChange) resolve(name string) (string, error) {
	if c.root == "" {
		return "", fmt.Errorf("nfsserve: no share directory to write attributes in")
	}
	root := filepath.Clean(c.root)
	target := filepath.Join(root, filepath.FromSlash(name))
	prefix := strings.TrimSuffix(root, string(filepath.Separator)) + string(filepath.Separator)
	if target != root && !strings.HasPrefix(target, prefix) {
		return "", fmt.Errorf("nfsserve: %q leaves the share", name)
	}
	return target, nil
}

// leavesRoot reports whether a relative path from filepath.Rel climbs out.
func leavesRoot(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
