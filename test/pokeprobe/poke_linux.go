//go:build linux

package main

import (
	"fmt"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// poke performs one primitive and returns any detail worth logging.
func poke(primitive, path string) (string, error) {
	switch primitive {
	case "openclose":
		// The event mask for a close comes from the file's open mode, not
		// from whether anything was written:
		//
		//	mask = (file->f_mode & FMODE_WRITE) ? FS_CLOSE_WRITE
		//	                                    : FS_CLOSE_NOWRITE;
		//
		// so O_WRONLY alone yields IN_OPEN + IN_CLOSE_WRITE having touched
		// nothing. O_TRUNC would also work and would destroy the file; it is
		// never correct here.
		//
		// This is also the only primitive that is silent over NFSv3, which
		// is stateless and has no OPEN operation at all, so it cannot echo
		// back to the client as a filesystem change.
		fd, err := unix.Open(path, unix.O_WRONLY, 0)
		if err != nil {
			return "", fmt.Errorf("open: %w", err)
		}
		if err := unix.Close(fd); err != nil {
			return "", fmt.Errorf("close: %w", err)
		}
		return "", nil

	case "mtime":
		return setMtime(path)

	case "dirmtime":
		return setMtime(filepath.Dir(path))

	case "touch":
		// The naive version, and a control rather than a candidate. Setting
		// BOTH times takes the first branch of fsnotify_change:
		//
		//	if ((ia_valid & (ATTR_ATIME|ATTR_MTIME)) == (ATTR_ATIME|ATTR_MTIME))
		//	        mask |= FS_ATTRIB;
		//
		// giving IN_ATTRIB and NOT IN_MODIFY. Most watchers key on IN_MODIFY
		// or IN_CLOSE_WRITE, which is why every touch-based workaround in the
		// wild is unreliable.
		ts := []unix.Timespec{
			{Sec: 0, Nsec: unix.UTIME_NOW},
			{Sec: 0, Nsec: unix.UTIME_NOW},
		}
		if err := unix.UtimesNanoAt(unix.AT_FDCWD, path, ts, 0); err != nil {
			return "", fmt.Errorf("utimensat: %w", err)
		}
		return "", nil

	case "create":
		fd, err := unix.Open(path, unix.O_WRONLY|unix.O_CREAT, 0o644)
		if err != nil {
			return "", fmt.Errorf("open: %w", err)
		}
		if err := unix.Close(fd); err != nil {
			return "", fmt.Errorf("close: %w", err)
		}
		return "", nil

	case "unlink":
		if err := unix.Unlink(path); err != nil {
			return "", fmt.Errorf("unlink: %w", err)
		}
		return "", nil

	case "stat":
		var st unix.Stat_t
		if err := unix.Stat(path, &st); err != nil {
			return "", fmt.Errorf("stat: %w", err)
		}
		// st_dev is the question behind the whole design: if the volume
		// mountpoint and the container's view of the same file report the
		// same device, they share a superblock and therefore an inode, and a
		// poke through either is seen by a watcher on the other.
		return fmt.Sprintf("dev=%d ino=%d", st.Dev, st.Ino), nil
	}

	return "", fmt.Errorf("unknown primitive %q", primitive)
}

// setMtime rewrites a path's mtime to the value it already has.
//
// Leaving atime as UTIME_OMIT drops ATTR_ATIME from ia_valid, so
// fsnotify_change falls past the both-times branch to:
//
//	else if (ia_valid & ATTR_MTIME)   mask |= FS_MODIFY;
//
// which is a real IN_MODIFY. Writing back the CURRENT mtime means nothing
// observable changes -- no build system sees a newer file, and the SETATTR
// this produces over NFS is an identity the server can decline to apply.
func setMtime(path string) (string, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return "", fmt.Errorf("stat: %w", err)
	}
	ts := []unix.Timespec{
		{Sec: 0, Nsec: unix.UTIME_OMIT},
		st.Mtim,
	}
	if err := unix.UtimesNanoAt(unix.AT_FDCWD, path, ts, 0); err != nil {
		return "", fmt.Errorf("utimensat: %w", err)
	}
	return fmt.Sprintf("mtime=%d.%09d", st.Mtim.Sec, st.Mtim.Nsec), nil
}
