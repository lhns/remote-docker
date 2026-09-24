package fswatch

import "strings"

// An event path is matched to its share component by component, under the
// local filesystem's case rules, and the tail is rebuilt from the EVENT's
// spelling. filepath.Rel compares bytes, and a root typed as `c:\projects\Foo`
// against events reported as `C:\Projects\Foo\...` silently matched nothing.
// Lowercasing and slicing by length is wrong too: ToLower is not
// length-preserving (U+0130). workspace.CanonicalKey is for identity, not this.

// splitLocal normalises separators and strips Windows extended-length
// prefixes, then splits into components. It never changes case: the result
// rebuilds a path on a case-sensitive filesystem.
func splitLocal(goos, p string) []string {
	if goos == "windows" {
		p = strings.ReplaceAll(p, `\`, "/")

		// \\?\C:\x and \\?\UNC\server\share match their plain spelling.
		if rest, ok := strings.CutPrefix(p, "//?/"); ok {
			if unc, isUNC := strings.CutPrefix(rest, "UNC/"); isUNC {
				p = "//" + unc
			} else {
				p = rest
			}
		}
	}

	// Empty components are dropped: a doubled separator in an event path would
	// fail workspace validation, and components are only rejoined singly.
	parts := make([]string, 0, 8)
	for part := range strings.SplitSeq(p, "/") {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	return parts
}

// foldEqual compares two path components under the CLIENT filesystem's case
// rules.
func foldEqual(goos, a, b string) bool {
	if caseInsensitive(goos) {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// caseInsensitive reports whether the local filesystem folds case.
func caseInsensitive(goos string) bool {
	return goos == "windows" || goos == "darwin"
}

// relativeTo returns the export-relative path of local under the share rooted
// at rootParts ("/" for the root itself), and whether it is under it at all.
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
