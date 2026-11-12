//go:build linux

package nofollow

import (
	"errors"
	"fmt"
	"os"
	"path"
	"strings"

	"golang.org/x/sys/unix"
)

// ErrLink is what a path with a symbolic link in it is refused with.
var ErrLink = errors.New("a symbolic link is in the path")

// Open returns an O_PATH descriptor for rel below the directory trusted. The
// caller closes it.
//
// trusted is opened as given, so a link in it (/proc/<pid>/root is one) is
// followed: it names a place the agent chose. Every component of rel is then
// opened relative to the previous one with O_NOFOLLOW, so a link in any of them
// is an error rather than a step, an absolute one included, and ".." never
// leaves trusted because rel is cleaned as a rooted path first.
//
// O_PATH, because it does not open the file: no FIFO waiting for a reader, no
// device driver. The descriptor can be fstat-ed, used as the
// directory of a further openat and named through /proc/self/fd/N.
func Open(trusted, rel string) (int, error) {
	fd, err := unix.Open(trusted, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, &os.PathError{Op: "open", Path: trusted, Err: err}
	}
	rel = path.Clean("/" + rel)
	parts := strings.FieldsFunc(rel, func(r rune) bool { return r == '/' })
	for i, name := range parts {
		flags := unix.O_PATH | unix.O_NOFOLLOW | unix.O_CLOEXEC
		if i < len(parts)-1 {
			flags |= unix.O_DIRECTORY
		}
		next, err := unix.Openat(fd, name, flags, 0)
		_ = unix.Close(fd)
		if err != nil {
			return -1, &os.PathError{Op: "openat", Path: rel, Err: err}
		}
		fd = next
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return -1, &os.PathError{Op: "fstat", Path: rel, Err: err}
	}
	if st.Mode&unix.S_IFMT == unix.S_IFLNK {
		_ = unix.Close(fd)
		return -1, &os.PathError{Op: "open", Path: rel, Err: ErrLink}
	}
	return fd, nil
}

// Dev is the device number of rel below trusted, and of its parent directory,
// without following a link. They differ when something is mounted at rel.
func Dev(trusted, rel string) (self, parent uint64, err error) {
	rel = path.Clean("/" + rel)
	for i, p := range []string{rel, path.Dir(rel)} {
		fd, err := Open(trusted, p)
		if err != nil {
			return 0, 0, err
		}
		var st unix.Stat_t
		err = unix.Fstat(fd, &st)
		_ = unix.Close(fd)
		if err != nil {
			return 0, 0, fmt.Errorf("fstat %s: %w", p, err)
		}
		if i == 0 {
			self = uint64(st.Dev) //nolint:unconvert // uint32 on some architectures
		} else {
			parent = uint64(st.Dev) //nolint:unconvert // uint32 on some architectures
		}
	}
	return self, parent, nil
}

// ReadDir names what is in the directory rel below trusted, without following a
// link. Names only: an entry's type would be looked up by a path that is not
// real, where the filesystem does not report it.
func ReadDir(trusted, rel string) ([]string, error) {
	pfd, err := Open(trusted, rel)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(pfd) }()
	// "." of an O_PATH directory descriptor is that directory, and unlike the
	// descriptor it can be read.
	dfd, err := unix.Openat(pfd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: rel, Err: err}
	}
	f := os.NewFile(uintptr(dfd), rel)
	defer func() { _ = f.Close() }()
	return f.Readdirnames(-1)
}
