package workspace

import (
	"fmt"
	"strings"
)

// MaxAccountNameLength is the longest account name the workspace will create.
// Linux allows 32; 30 leaves room and matches what the shell implementation
// used, so existing deployments derive the same names.
const MaxAccountNameLength = 30

// AccountName derives an account name from a key file's base name, on the
// workspace from its file in authorized_keys.d and on the client from the
// local username. Copies that drift present as an account that does not exist
// where it plainly does.
//
// The rules are the shell implementation's `sanitize`: a different name for
// the same file strands the old home directory and changes the uid, and with
// it the reverse-tunnel port. An underivable name is an error, which the
// workspace refuses and the client replaces.
func AccountName(base string) (string, error) {
	var b strings.Builder
	for _, r := range strings.ToLower(base) {
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
	if len(name) > MaxAccountNameLength {
		name = name[:MaxAccountNameLength]
	}
	return name, nil
}
