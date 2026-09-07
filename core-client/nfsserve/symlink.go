package nfsserve

import "github.com/lhns/remote-docker/core/logx"

// Why a symlink is refused on Windows, and why nothing stands in for one.
//
// Windows needs SeCreateSymbolicLinkPrivilege to create a symlink. Developer
// Mode grants it and an elevated process holds it; an ordinary account has
// neither, so os.Symlink fails with ERROR_PRIVILEGE_NOT_HELD and a share
// refuses the request. `npm i` of any package with a bin entry ends there.
//
// A HARD LINK is the obvious substitute, since os.Link needs no privilege. It
// was built and measured on 2026-09-07 and it cannot be done over NFSv3:
// answering SYMLINK with success is answering that a symlink exists, so the
// Linux client holds the inode as one and serves the target IT sent as the
// file's contents, whatever the server reports the type to be afterwards.
//
//	cat bin/prog   ->  ../lib/          (the target, cut to the file's length)
//	stat bin/prog  ->  regular file, nlink=2
//
// It survived an attribute timeout, a new container and a fresh mount, and a
// hard link made on this machine rather than through the share read correctly
// in the same container. So nothing is being corrupted: the protocol has no
// way to answer "I made something other than what you asked for", and silently
// serving the wrong bytes is worse than a refusal that names its remedy.

// warnPrivilege says why the host refused, once per share: npm creates a bin
// entry per package, and a hundred identical warnings is a message nobody
// reads. The share is rebuilt on every connect, so a reconnect says it again,
// which is the right cadence for something the user is meant to act on.
func (n *noFollowFS) warnPrivilege() {
	n.warned.Do(func() {
		logx.Or(n.log).Warn("nfs: this machine cannot create a symlink; Windows withholds the privilege" +
			"\n  fix: enable Developer Mode in Windows settings, or run the client elevated")
	})
}
