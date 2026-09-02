package workspace

// How an in-share path is spelled on the wire.
//
// Here rather than in either protocol package because BOTH ask it -- notify
// names paths to poke, cache names paths to fill and drop -- and a path
// accepted by one and refused by the other is a share that half works.

import (
	"fmt"
	"strings"
)

// ValidSharePath enforces the wire spelling of an in-share path.
//
// Deliberately a whitelist rather than a blacklist, and deliberately not
// path.Clean: cleaning would *repair* a traversal into something plausible
// instead of refusing it, so "/a/../../etc/shadow" would arrive as a path the
// agent happily touches. A malformed path is a bug worth reporting, never one
// worth guessing about.
func ValidSharePath(p string) error {
	switch {
	case p == "":
		return fmt.Errorf("workspace: share path is empty")
	case !strings.HasPrefix(p, "/"):
		return fmt.Errorf("workspace: share path %q is not absolute within its share", p)
	case strings.Contains(p, `\`):
		// A backslash is a path separator on the client and an ordinary
		// filename character in the workspace, so letting one through would
		// create a file whose name contains the rest of the path.
		return fmt.Errorf("workspace: share path %q contains a backslash; separators must be normalised", p)
	case strings.ContainsRune(p, 0):
		return fmt.Errorf("workspace: share path %q contains a NUL", p)
	}
	if p == "/" {
		return nil
	}
	for part := range strings.SplitSeq(strings.TrimPrefix(p, "/"), "/") {
		switch part {
		case "":
			return fmt.Errorf("workspace: share path %q has an empty component", p)
		case ".", "..":
			return fmt.Errorf("workspace: share path %q has a %q component", p, part)
		}
	}
	return nil
}
