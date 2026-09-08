package workspace

import (
	"fmt"
	"strings"
)

// MaxAccountNameLength is the longest account name the workspace will create.
// Linux allows 32; 30 leaves room and matches what the shell implementation
// used, so existing deployments derive the same names.
const MaxAccountNameLength = 30

// AccountName derives an account name from a key file's base name.
//
// Both ends derive it, so it is one function here rather than two copies that
// look alike. The workspace names an account from its file in
// authorized_keys.d; the client makes the same derivation from the local
// username to guess who to log in as when nothing says. Copies that drift
// present as an account that does not exist on a workspace where it plainly
// does.
//
// The rules are the shell implementation's `sanitize`, because a deployment
// upgrading to the agent must derive the same account for the same file --
// deriving a different one would strand the old account's home directory and
// hand the user a new uid, and with the uid a new reverse-tunnel port.
//
// A name that cannot be derived is an error rather than a substitute, because
// the two callers answer that differently: the workspace refuses the file, and
// the client falls back to a name the user can correct.
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
