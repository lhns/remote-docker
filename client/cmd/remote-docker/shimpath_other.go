//go:build !windows

package main

// PATH on Unix, where this program deliberately does NOT edit anything.
//
// There is no per-user environment to write. PATH comes from a shell's
// configuration, and which file that is depends on the shell, on whether the
// session is a login one, and on what the user has already arranged --
// bash reads .bashrc or .bash_profile depending, zsh reads .zshrc, fish uses
// its own syntax entirely, and any of them may source something else again.
// Guessing wrong writes a line that never runs, or duplicates one that does.
//
// ~/.local/bin is on PATH already on most systems, so the honest thing is to
// check, and print the one line to add when it is not.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// ensurePATH reports whether dir is reachable, and never changes anything --
// so it never reports having added it.
func ensurePATH(out io.Writer, dir string) (bool, error) {
	return false, reportPATH(out, dir)
}

// removePATH has nothing to undo, because ensurePATH did nothing.
func removePATH(io.Writer, string) error { return nil }

func reportPATH(out io.Writer, dir string) error {
	if onPath(dir) {
		_, _ = fmt.Fprintf(out, "%s is on your PATH.\n", dir)
		return nil
	}
	_, _ = fmt.Fprintf(out,
		"\n%s is NOT on your PATH. Add this to your shell's startup file:\n\n"+
			"    export PATH=\"%s:$PATH\"\n\n"+
			"(nothing here edits that file for you: which one it is depends on your shell)\n",
		dir, dir)
	return nil
}

// crossDeviceNote explains a hardlink that could not be made because the two
// paths are on different filesystems.
//
// A hardlink is a second directory entry for ONE inode, and an inode number
// means nothing outside its own filesystem -- which is what EXDEV says.
func crossDeviceNote(self, path string) (string, bool) {
	a, ok1 := deviceOf(self)
	b, ok2 := deviceOf(filepath.Dir(path))
	if !ok1 || !ok2 || a == b {
		return "", false
	}
	return fmt.Sprintf(
		"%s and %s are on different filesystems, and a hardlink cannot span them\n"+
			"(it is a second name for one inode, and an inode belongs to one filesystem).",
		self, filepath.Dir(path)), true
}

func deviceOf(path string) (uint64, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Dev), true
}
