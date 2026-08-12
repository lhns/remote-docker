package accounts

import (
	"fmt"
	"strings"
)

// maxNameLength is the longest account name we will create. Linux allows 32;
// 30 leaves room and matches what the shell implementation used, so existing
// deployments derive the same names.
const maxNameLength = 30

// SanitizeName derives a unix account name from a key file's base name.
//
// The rules match the shell implementation's `sanitize`, because a deployment
// upgrading to the agent must derive the same account for the same file --
// deriving a different one would strand the old account's home directory and
// hand the user a new uid, and therefore a new port.
//
// Unlike the shell version, a name that cannot be derived is an error rather
// than an empty string. The caller refuses the file instead of silently
// skipping it.
func SanitizeName(base string) (string, error) {
	lowered := strings.ToLower(base)

	var b strings.Builder
	for _, r := range lowered {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}

	// A unix account name must start with a letter or underscore.
	name := strings.TrimLeft(b.String(), "0123456789-")
	if name == "" {
		return "", fmt.Errorf("no usable account name can be derived from %q", base)
	}
	if len(name) > maxNameLength {
		name = name[:maxNameLength]
	}
	return name, nil
}
