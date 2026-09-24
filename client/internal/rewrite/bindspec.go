// Package rewrite turns bind mounts that name paths on this machine into
// Docker volumes backed by the client's NFS export (ADR 0006).
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

	// Options is the raw trailing field ("ro", "rw,z"), carried verbatim:
	// dropping one silently changes the mount.
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

// ParseBind splits a bind specification, where a colon may also belong to a
// Windows drive letter: `C:\projects:/app:ro` has three fields.
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

// resolveFields picks the reading of a leading `x:` whose target starts with
// a slash, as a Linux container path must: `c:/app` is volume `c` at /app.
func resolveFields(spec string) []string {
	drive := splitBind(spec)
	if len(drive) >= 2 && strings.HasPrefix(drive[1], "/") {
		return drive
	}
	if plain := strings.Split(spec, ":"); len(plain) >= 2 && strings.HasPrefix(plain[1], "/") {
		return plain
	}
	return drive
}

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
		// Drive letter: one letter, the colon, a separator.
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
// rather than a named volume, by Docker's rule: anything path-shaped is a path.
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
