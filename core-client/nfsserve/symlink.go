package nfsserve

// Why a symlink is refused on Windows, and why nothing stands in for one.
//
// Windows needs SeCreateSymbolicLinkPrivilege to create a symlink. Developer
// Mode grants it and an elevated process holds it; an ordinary account has
// neither, so os.Symlink fails with ERROR_PRIVILEGE_NOT_HELD and a share
// refuses the request. `npm i` of any package with a bin entry ends there.
//
// A HARD LINK looks like the obvious substitute: os.Link needs no privilege,
// and the case that matters is a link to a file that already exists. It was
// built and measured on 2026-09-07, and it cannot be done over NFSv3.
//
// Answering SYMLINK with success is answering that a symlink exists. The Linux
// client then holds that inode as a symlink and serves the target IT sent as
// the file's contents, whatever the server later reports the type to be:
//
//	cat bin/prog   ->  ../lib/          (the target, cut to the file's length)
//	stat bin/prog  ->  regular file, nlink=2
//	the same file on this machine, and through any other name  ->  payload
//
// It survived an attribute timeout, a new container and a fresh mount, and a
// hard link made on this machine instead of through the share reads correctly
// in the same container. So the share is not corrupting anything: the protocol
// has no way to answer "I made something other than what you asked for", and a
// mode that silently serves the wrong bytes is worse than a refusal that names
// its remedy.
//
// The remedy is Developer Mode, and it is the whole of what this file offers.

// fixSymlinkPrivilege is the remedy, in one place because the message names it.
const fixSymlinkPrivilege = "\n  fix: enable Developer Mode in Windows settings, or run the client elevated"

// warnPrivilege says why the host refused, once per share.
//
// Once, because a tool that links once links a hundred times: npm creates a bin
// entry per package. The share is rebuilt on every connect, so this is said
// again on a reconnect, which is the right cadence for something the user is
// meant to act on.
func (n *noFollowFS) warnPrivilege() {
	n.warned.Do(func() {
		n.logger().Warn("nfs: this machine cannot create a symlink; Windows withholds the privilege" + fixSymlinkPrivilege)
	})
}
