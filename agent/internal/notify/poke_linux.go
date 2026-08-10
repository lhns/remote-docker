//go:build linux

package notify

import (
	"golang.org/x/sys/unix"
)

// SyscallPoker makes a watcher notice a path, without changing anything about
// it.
//
// Which syscalls, and why exactly these, is measured rather than reasoned
// about: test/integration.sh section 11d runs the matrix on every push, and
// ADR 0016 records it. In short:
//
//	utimensat with atime=UTIME_OMIT   IN_MODIFY
//	utimensat with both times set     IN_ATTRIB only  <- the naive "touch"
//	open(O_WRONLY) + close()          IN_OPEN, IN_CLOSE_WRITE
//
// Most watchers key on IN_MODIFY or IN_CLOSE_WRITE and ignore IN_ATTRIB, which
// is why every touch-based workaround in the wild is unreliable and why the
// UTIME_OMIT branch below is the whole point.
type SyscallPoker struct{}

// O_NOFOLLOW and AT_SYMLINK_NOFOLLOW below are load-bearing, and they did not
// used to be.
//
// While every path was resolved inside the agent's own filesystem, refusing to
// follow a symlink was tidiness, since the client does not watch through them
// either, so following one would replay somewhere the client never meant. With
// a daemon per account (ADR 0019) these paths are reached through
// /proc/<pid>/root of somebody else's container, so a symlink here is a symlink
// planted in a filesystem this process does not control, pointing anywhere it
// likes. This is a root process being told which path to touch; following one
// would be the escape.
func (SyscallPoker) Poke(path string, isDir bool) error {
	if err := pokeMtime(path); err != nil {
		return err
	}
	if isDir {
		// O_WRONLY on a directory is EISDIR. The utimensat above already
		// produced IN_MODIFY|IN_ISDIR, which is what a rescanning watcher
		// needs.
		return nil
	}

	// The close event's mask comes from the file's open mode, not from
	// whether anything was written:
	//
	//	mask = (file->f_mode & FMODE_WRITE) ? FS_CLOSE_WRITE : FS_CLOSE_NOWRITE;
	//
	// so O_WRONLY alone yields IN_CLOSE_WRITE having touched nothing. O_TRUNC
	// would produce it too, and would destroy the file; it is never correct
	// here. Nor is O_CREAT: this must never bring a path into existence.
	//
	// It is also completely silent over NFSv3, which is stateless and has no
	// OPEN operation, so this primitive cannot echo back to the client as a
	// change, which would otherwise loop forever.
	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		// A read-only file, or one we may not open for writing, still got its
		// IN_MODIFY above. Not worth failing the whole event for.
		return nil
	}
	return unix.Close(fd)
}

// pokeMtime rewrites a path's mtime to the value it already has.
//
// Leaving atime as UTIME_OMIT drops ATTR_ATIME from ia_valid, so
// fsnotify_change falls past the both-times branch to
//
//	else if (ia_valid & ATTR_MTIME)   mask |= FS_MODIFY;
//
// which is a genuine IN_MODIFY. Writing back the CURRENT mtime means nothing
// observable changes: no build system sees a newer file, and the SETATTR this
// produces over NFS is an identity the client's own server can decline to
// apply, which is what keeps the echo loop closed.
func pokeMtime(path string) error {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT == unix.S_IFLNK {
		// Never follow a symlink out of the share. The client does not watch
		// through them either.
		return nil
	}
	ts := []unix.Timespec{
		{Sec: 0, Nsec: unix.UTIME_OMIT},
		st.Mtim,
	}
	return unix.UtimesNanoAt(unix.AT_FDCWD, path, ts, unix.AT_SYMLINK_NOFOLLOW)
}
