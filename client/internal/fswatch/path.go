package fswatch

import "strings"

// Translating a local path into the export-relative form the agent expects is
// the one place in this package where a plausible-looking shortcut is wrong on
// two of the three platforms we ship.
//
// filepath.Rel compares bytes on every OS. But a share root comes from the
// user's command line (`c:\projects\Foo`) while events come back from
// ReadDirectoryChangesW spelled the way the directory actually is on disk --
// `C:\Projects\Foo\src\a.ts`. Rel either fails or returns nonsense, and the
// failure is silent: no events for that share, no error anywhere.
//
// Nor can we lowercase both and slice by length. strings.ToLower is not
// length-preserving in Unicode (U+0130 becomes two runes), so the offset is
// wrong for exactly the paths that are hardest to notice.
//
// So: split both sides into components, compare component-wise under the local
// filesystem's own case rules, and rebuild the tail from the EVENT's
// components. The workspace's filesystem is case-sensitive; sending the root's
// casing instead of the file's would name a path that is not there.
//
// workspace.CanonicalKey solves a neighbouring problem -- whether two paths
// are the same directory, and lowercases to do it. It is the right function
// for identity and the wrong one here.

// splitLocal normalises separators and strips Windows extended-length
// prefixes, then splits into components. It never changes the case of a
// component: the result is used both for comparison and for rebuilding a path
// that must exist on a case-sensitive filesystem.
func splitLocal(goos, p string) []string {
	if goos == "windows" {
		p = strings.ReplaceAll(p, `\`, "/")

		// \\?\C:\x and \\?\UNC\server\share are extended-length spellings of
		// paths we otherwise handle; strip the prefix so they match the plain
		// spelling of the same directory.
		if rest, ok := strings.CutPrefix(p, "//?/"); ok {
			if unc, isUNC := strings.CutPrefix(rest, "UNC/"); isUNC {
				p = "//" + unc
			} else {
				p = rest
			}
		}
	}

	// Empty components are dropped rather than preserved, which collapses a
	// UNC path's leading "//" and any doubled separator. Components are only
	// ever compared with each other and rejoined with single separators, so
	// nothing downstream can tell the difference, and a doubled separator
	// in an event path would otherwise fail workspace validation.
	parts := make([]string, 0, 8)
	for part := range strings.SplitSeq(p, "/") {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	return parts
}

// foldEqual compares two path components under the local filesystem's case
// rules: exact on Linux, case-insensitive on Windows and macOS.
//
// This describes the CLIENT's filesystem, which is the only one that can tell
// us whether two spellings name the same file. The workspace's filesystem is
// always case-sensitive and is not consulted.
func foldEqual(goos, a, b string) bool {
	if goos == "windows" || goos == "darwin" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// caseInsensitive reports whether the local filesystem folds case, which
// decides whether the watcher may treat two spellings as one directory.
func caseInsensitive(goos string) bool {
	return goos == "windows" || goos == "darwin"
}

// relativeTo returns the export-relative POSIX path of local under the share
// rooted at rootParts, and whether local is under that root at all.
//
// The share root itself is "/". Every other result has a leading slash, "/"
// separators, and the casing the event reported rather than the casing the
// root was spelled with.
func relativeTo(goos string, rootParts []string, local string) (string, bool) {
	parts := splitLocal(goos, local)
	if len(parts) < len(rootParts) {
		return "", false
	}
	for i, want := range rootParts {
		if !foldEqual(goos, parts[i], want) {
			return "", false
		}
	}
	if len(parts) == len(rootParts) {
		return "/", true
	}
	return "/" + strings.Join(parts[len(rootParts):], "/"), true
}
