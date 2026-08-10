// Package rewrite turns bind mounts that name paths on this machine into
// Docker volumes backed by the client's NFS export.
//
// This is the translation ADR 0006 rests on. The workspace daemon cannot see
// the client's filesystem, so `-v D:\data:/data` has to become a volume the
// daemon can mount for itself -- from the tunnelled NFS server, in its own
// namespace, when the container starts.
package rewrite

import (
	"fmt"
	"strings"
)

// BindSpec is a parsed `-v` argument.
type BindSpec struct {
	// Source is the host side. Empty means an anonymous volume.
	Source string

	// Target is the path inside the container.
	Target string

	// Options is the raw trailing field -- "ro", "rw,z", "cached" and so on.
	// It is carried through untouched rather than parsed: the daemon
	// understands more of them than we need to, and dropping one silently
	// changes the mount.
	Options string
}

// String renders the spec back into `-v` form.
func (b BindSpec) String() string {
	parts := make([]string, 0, 3)
	if b.Source != "" {
		parts = append(parts, b.Source)
	}
	parts = append(parts, b.Target)
	if b.Options != "" {
		parts = append(parts, b.Options)
	}
	return strings.Join(parts, ":")
}

// ParseBind splits a bind specification.
//
// The colon is both the field separator and part of a Windows drive letter,
// which is the entire difficulty here: `C:\projects:/app:ro` has four colons
// and three fields. A drive letter is recognised only where it can appear --
// a single letter, followed by a colon, followed by a separator, so a named
// volume called `c` is not mistaken for one.
func ParseBind(spec string) (BindSpec, error) {
	if strings.TrimSpace(spec) == "" {
		return BindSpec{}, fmt.Errorf("rewrite: empty bind specification")
	}

	fields := resolveFields(spec)
	switch len(fields) {
	case 1:
		// An anonymous volume: `-v /data`.
		return BindSpec{Target: fields[0]}, nil
	case 2:
		return BindSpec{Source: fields[0], Target: fields[1]}, nil
	case 3:
		return BindSpec{Source: fields[0], Target: fields[1], Options: fields[2]}, nil
	default:
		return BindSpec{}, fmt.Errorf("rewrite: %q has %d fields, want at most 3", spec, len(fields))
	}
}

// resolveFields splits a spec, choosing between the two readings of a leading
// `x:` when both are syntactically possible.
//
// `c:/app` is character-for-character a drive-rooted path and also the volume
// `c` mounted at `/app`; nothing in the string distinguishes them. What does
// distinguish them is the target: the workspace is Linux, so the container
// path is always absolute and starts with a slash. Reading `c:/app` as a drive
// leaves no target at all, and reading `c:/app:ro` as one makes the target
// "ro" -- both impossible, so the volume reading is correct.
//
// A path spelled `C:\projects:/app` has no such ambiguity, and the drive
// reading is the only one that yields a usable target.
func resolveFields(spec string) []string {
	drive := splitBind(spec)
	if len(drive) >= 2 && strings.HasPrefix(drive[1], "/") {
		return drive
	}
	if plain := splitPlain(spec); len(plain) >= 2 && strings.HasPrefix(plain[1], "/") {
		return plain
	}
	return drive
}

// splitPlain splits on every colon, with no drive-letter handling.
func splitPlain(spec string) []string { return strings.Split(spec, ":") }

// splitBind splits on colons that are not part of a drive letter.
func splitBind(spec string) []string {
	var (
		fields []string
		cur    strings.Builder
	)
	for i := 0; i < len(spec); i++ {
		if spec[i] != ':' {
			cur.WriteByte(spec[i])
			continue
		}
		// A drive letter is exactly one alphabetic character followed by the
		// colon and then a separator. Anything else is a field boundary.
		if cur.Len() == 1 && isAlpha(cur.String()[0]) && i+1 < len(spec) && isSeparator(spec[i+1]) {
			cur.WriteByte(':')
			continue
		}
		fields = append(fields, cur.String())
		cur.Reset()
	}
	return append(fields, cur.String())
}

// IsLocalPath reports whether a bind source names a path on this machine
// rather than a named volume.
//
// Docker's own rule: anything that looks like a path is one, and everything
// else is a volume name. Getting this wrong in the permissive direction would
// try to export a named volume as a directory; in the restrictive direction it
// would ship a client path to a daemon that cannot see it, which is the
// original bug this project exists to fix.
func IsLocalPath(source string) bool {
	switch {
	case source == "":
		return false
	case strings.HasPrefix(source, "/"):
		return true
	case source == "." || source == "..":
		return true
	case strings.HasPrefix(source, "./") || strings.HasPrefix(source, "../"):
		return true
	case strings.HasPrefix(source, `.\`) || strings.HasPrefix(source, `..\`):
		return true
	case strings.HasPrefix(source, `\\`):
		// UNC share.
		return true
	case len(source) >= 3 && isAlpha(source[0]) && source[1] == ':' && isSeparator(source[2]):
		// Windows drive-rooted path.
		return true
	default:
		return false
	}
}

func isAlpha(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isSeparator(c byte) bool { return c == '/' || c == '\\' }
