package main

// The PATH string arithmetic, kept away from the registry so it can be tested
// on any machine -- and because getting it wrong damages something the user
// cannot easily reconstruct.

import (
	"os"
	"path/filepath"
	"strings"
)

// appendPath adds dir to a PATH value if it is not already there, and reports
// whether it changed anything.
//
// APPENDED, never prepended. If a real Docker is installed on this machine
// later, its entry comes first and wins -- which is the correct outcome: a
// local Docker is the thing this program stands in for, not something it
// should shadow.
//
// Comparison is case-insensitive and ignores a trailing separator, because
// `C:\Users\me\bin`, `c:\users\me\bin\` and `C:/Users/me/bin` are one
// directory and adding it three times is how a PATH becomes unreadable.
func appendPath(current, dir string) (string, bool) {
	if hasPathEntry(current, dir) {
		return current, false
	}
	trimmed := strings.TrimRight(current, string(filepath.ListSeparator))
	if trimmed == "" {
		return dir, true
	}
	return trimmed + string(filepath.ListSeparator) + dir, true
}

// removeFromPath takes every occurrence of dir out, and reports whether it
// found any.
//
// Every occurrence, not the first: a PATH that accumulated the same directory
// twice is exactly the state this should leave clean.
func removeFromPath(current, dir string) (string, bool) {
	var (
		kept    []string
		removed bool
	)
	for _, entry := range strings.Split(current, string(filepath.ListSeparator)) {
		if entry != "" && samePathEntry(entry, dir) {
			removed = true
			continue
		}
		kept = append(kept, entry)
	}
	if !removed {
		return current, false
	}

	// Empty entries are kept, every one of them. An empty entry in PATH means
	// the CURRENT DIRECTORY -- so dropping one while tidying up changes what
	// the user's shell resolves, quietly, in a way that looks like nothing
	// happened. Nothing here is entitled to remove anything but our own entry.
	return strings.Join(kept, string(filepath.ListSeparator)), true
}

// onPath reports whether this process's own PATH already reaches dir.
//
// The process's PATH rather than the registry's, deliberately: this is what
// the shell the user is standing in will search, and on Windows that is
// exactly the value that does NOT change when the registry does.
func onPath(dir string) bool { return hasPathEntry(os.Getenv("PATH"), dir) }

func hasPathEntry(current, dir string) bool {
	for _, entry := range strings.Split(current, string(filepath.ListSeparator)) {
		if entry != "" && samePathEntry(entry, dir) {
			return true
		}
	}
	return false
}

// samePathEntry compares two PATH entries as directories.
//
// Case-insensitively on Windows only. Two paths differing in case are the same
// directory there and are two different directories on Linux, and treating
// them alike either way would be wrong on one of the platforms this ships to.
func samePathEntry(a, b string) bool {
	a, b = filepath.Clean(strings.TrimSpace(a)), filepath.Clean(strings.TrimSpace(b))
	if filepath.Separator == '\\' {
		return strings.EqualFold(a, b)
	}
	return a == b
}
