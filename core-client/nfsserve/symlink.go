package nfsserve

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"syscall"
)

// SymlinkMode is what a share does when a container creates a symlink.
//
// Windows refuses the call outright unless the client holds
// SeCreateSymbolicLinkPrivilege, which an ordinary account does not: Developer
// Mode grants it, and an elevated process has it. Everything else about a
// share works there, so a single privilege stops `npm i` of any package with a
// bin entry, and every other tool that links.
type SymlinkMode string

const (
	// SymlinkNative creates a symlink, and fails where the host will not.
	SymlinkNative SymlinkMode = "native"

	// SymlinkHardlink creates a hard link to the file the target names.
	//
	// A share stops telling the truth here, which is why it is opt-in: the
	// name reads back as an ordinary file with two links, readlink fails, and
	// removing the target leaves the content reachable through the link. It
	// buys the case that matters, a link to a file that already exists.
	SymlinkHardlink SymlinkMode = "hardlink"
)

// ParseSymlinkMode reads the setting. Empty is the default.
func ParseSymlinkMode(s string) (SymlinkMode, error) {
	switch s {
	case "":
		return SymlinkNative, nil
	case string(SymlinkNative):
		return SymlinkNative, nil
	case string(SymlinkHardlink):
		return SymlinkHardlink, nil
	}
	return "", fmt.Errorf("%q is not a symlink mode; want %s or %s",
		s, SymlinkNative, SymlinkHardlink)
}

// fixSymlinkPrivilege is the remedy, in one place because the message names it
// and so does the refusal a translation cannot serve.
const fixSymlinkPrivilege = "\n  fix: enable Developer Mode in Windows settings, or set symlinks=hardlink for this workspace"

// hardLink serves a SYMLINK request with a hard link.
//
// The target is resolved the way the kernel would resolve the symlink: against
// the DIRECTORY holding the link, not against the share root. It must land on
// a regular file inside the share, and each way it cannot is refused with the
// reason rather than translated into something else.
//
// Containment is not a formality here. An untranslated symlink pointing out of
// the share is harmless, because every traversal of it resolves through
// SecureJoin and lands back inside; a HARD link to a file outside the share is
// that file, readable and writable through the share for as long as it exists.
func (n *noFollowFS) hardLink(target, link string) error {
	if path.IsAbs(target) || filepath.IsAbs(target) {
		// An absolute target is the container's path, which names nothing on
		// this machine: /app is meaningful only on the other side of the mount.
		return n.refuseLink(link, "its target is an absolute path, which names a file in the container rather than one in the share")
	}

	linkPath, err := n.leaf(link)
	if err != nil {
		return err
	}

	rel := n.relative(link)
	targetPath, err := secureLeaf(n.Root(), path.Join(filepath.ToSlash(filepath.Dir(rel)), target))
	if err != nil {
		return n.refuseLink(link, "its target is outside the share")
	}

	fi, err := os.Lstat(targetPath)
	switch {
	case os.IsNotExist(err):
		return n.refuseLink(link, "its target does not exist, and a hard link needs one")
	case err != nil:
		return err
	case fi.IsDir():
		return n.refuseLink(link, "its target is a directory, which cannot be hard linked")
	case fi.Mode()&os.ModeSymlink != 0:
		return n.refuseLink(link, "its target is itself a symlink")
	}

	return os.Link(targetPath, linkPath)
}

// refuseLink reports why a translation could not stand in, and returns the
// error the container sees. The reason only exists on this side: the wire
// carries an errno and nothing else.
func (n *noFollowFS) refuseLink(link, why string) error {
	n.logger().Warn("nfs: a symlink was refused; this share translates symlinks into hard links and " + why)
	return &fs.PathError{Op: "symlink", Path: link, Err: syscall.EPERM}
}

// warnPrivilege says why the host refused, once per share.
//
// Once, because a tool that links once links a hundred times: npm creates a
// bin entry per package. The share is rebuilt on every connect, so this is
// said again on a reconnect, which is the right cadence for something the user
// is meant to act on.
func (n *noFollowFS) warnPrivilege() {
	n.warned.Do(func() {
		n.logger().Warn("nfs: this machine cannot create a symlink; Windows withholds the privilege" + fixSymlinkPrivilege)
	})
}
