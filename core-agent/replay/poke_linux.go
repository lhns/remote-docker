//go:build linux

package replay

import (
	"strconv"

	"golang.org/x/sys/unix"

	"github.com/lhns/remote-docker/core-agent/nofollow"
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

// Poke touches rel, a path below the directory trusted, and follows no link in
// any component of rel.
//
// That is load-bearing. With a daemon per account (ADR 0019) rel is reached
// through /proc/<pid>/root of somebody else's container, so a link anywhere in
// it was planted in a filesystem this process does not control and points
// wherever it likes; this is a root process being told which path to touch, and
// following one would be the escape. With the shared daemon, trusted is the
// volume's mountpoint and rel is the user's share, whose links the user makes.
// So the path is walked with nofollow.Open, which refuses a link at every step,
// and the operations below act on the descriptor it returns, through
// /proc/self/fd, never on a name that could be swapped after the check.
func (SyscallPoker) Poke(trusted, rel string, isDir bool) error {
	fd, err := nofollow.Open(trusted, rel)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	// Names the inode fd holds, wherever the name now leads.
	self := "/proc/self/fd/" + strconv.Itoa(fd)

	regular, err := pokeMtime(fd, self)
	if err != nil {
		return err
	}
	if isDir || !regular {
		// A directory already got IN_MODIFY|IN_ISDIR above, which is what a
		// rescanning watcher needs. Anything else that is not a regular file
		// is not opened at all: a FIFO blocks the open until a reader comes,
		// and the notify channel replays one event at a time.
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
	//
	// No O_NOFOLLOW: self is itself a link, to the inode nofollow.Open reached,
	// and that inode was just seen to be a regular file.
	wfd, err := unix.Open(self, unix.O_WRONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		// A read-only file, or one we may not open for writing, still got its
		// IN_MODIFY above. Not worth failing the whole event for.
		return nil
	}
	return unix.Close(wfd)
}

// pokeMtime rewrites an inode's mtime to the value it already has.
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
//
// self is /proc/self/fd/N for fd, which is O_PATH and so cannot be given to
// futimens. It reports whether the inode is a regular file, the only kind Poke
// opens.
func pokeMtime(fd int, self string) (regular bool, err error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return false, err
	}
	ts := []unix.Timespec{
		{Sec: 0, Nsec: unix.UTIME_OMIT},
		st.Mtim,
	}
	return st.Mode&unix.S_IFMT == unix.S_IFREG,
		unix.UtimesNanoAt(unix.AT_FDCWD, self, ts, 0)
}
