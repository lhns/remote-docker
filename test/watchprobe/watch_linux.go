//go:build linux

package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// bits is every inotify event we care to name, in the order the kernel
// defines them. IN_ALL_EVENTS is requested rather than a curated mask: the
// point of this probe is to find out which events a poke actually produces,
// so filtering to the ones we expect would beg the question.
var bits = []struct {
	mask uint32
	name string
}{
	{unix.IN_ACCESS, "IN_ACCESS"},
	{unix.IN_MODIFY, "IN_MODIFY"},
	{unix.IN_ATTRIB, "IN_ATTRIB"},
	{unix.IN_CLOSE_WRITE, "IN_CLOSE_WRITE"},
	{unix.IN_CLOSE_NOWRITE, "IN_CLOSE_NOWRITE"},
	{unix.IN_OPEN, "IN_OPEN"},
	{unix.IN_MOVED_FROM, "IN_MOVED_FROM"},
	{unix.IN_MOVED_TO, "IN_MOVED_TO"},
	{unix.IN_CREATE, "IN_CREATE"},
	{unix.IN_DELETE, "IN_DELETE"},
	{unix.IN_DELETE_SELF, "IN_DELETE_SELF"},
	{unix.IN_MOVE_SELF, "IN_MOVE_SELF"},
	{unix.IN_UNMOUNT, "IN_UNMOUNT"},
	{unix.IN_Q_OVERFLOW, "IN_Q_OVERFLOW"},
	{unix.IN_IGNORED, "IN_IGNORED"},
	{unix.IN_ISDIR, "IN_ISDIR"},
}

func decode(mask uint32) string {
	var names []string
	for _, b := range bits {
		if mask&b.mask != 0 {
			names = append(names, b.name)
		}
	}
	if len(names) == 0 {
		return fmt.Sprintf("UNKNOWN(%#x)", mask)
	}
	return strings.Join(names, "|")
}

// watch establishes an inotify watch on dir and calls report for every event,
// with the mask decoded to its kernel names.
//
// The read loop runs on its own goroutine and is never unblocked -- inotify
// has no wakeup and the process is short-lived, so the blocked thread simply
// dies with it. Closing the descriptor from another goroutine to interrupt
// the read is a race the probe does not need to take.
func watch(dir string, report func(mask, name string)) (func(), error) {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC)
	if err != nil {
		return nil, fmt.Errorf("inotify_init1: %w", err)
	}
	if _, err := unix.InotifyAddWatch(fd, dir, unix.IN_ALL_EVENTS); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("inotify_add_watch %s: %w", dir, err)
	}

	go func() {
		buf := make([]byte, 16*1024)
		for {
			n, err := unix.Read(fd, buf)
			if err != nil {
				if err == unix.EINTR {
					continue
				}
				return
			}
			for off := 0; off+unix.SizeofInotifyEvent <= n; {
				raw := (*unix.InotifyEvent)(unsafe.Pointer(&buf[off]))
				name := ""
				if raw.Len > 0 {
					start := off + unix.SizeofInotifyEvent
					name = strings.TrimRight(string(buf[start:start+int(raw.Len)]), "\x00")
				}
				if name == "" {
					// An event on the watched directory itself. Name it, so
					// the log distinguishes "the directory changed" from "a
					// file in it changed" -- the coarse fallback depends on
					// exactly that difference.
					name = filepath.Base(dir) + "/"
				}
				report(decode(raw.Mask), name)
				off += unix.SizeofInotifyEvent + int(raw.Len)
			}
		}
	}()

	return func() { _ = unix.Close(fd) }, nil
}
