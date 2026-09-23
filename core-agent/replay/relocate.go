package replay

import (
	"fmt"
	"path"
)

// Relocate maps a mountpoint reported by another daemon into our filesystem.
//
// `path`, not `path/filepath`: these are Linux paths on both ends, and
// Windows-flavoured joining on the development machine would make the tests lie.
func Relocate(mp string, root func() (string, error)) (string, error) {
	if root == nil {
		return mp, nil
	}
	prefix, err := root()
	if err != nil {
		// Never fall back to the unrelocated path: the agent's own
		// /var/lib/docker is the SHARED daemon's, so it exists and would take
		// one account's edits into another daemon's volume.
		return "", fmt.Errorf("notify: locating the daemon holding the volume: %w", err)
	}
	// "" and "/" are both the identity. "/" through the join below would be
	// refused, since Under("/", p, "/") asks for a "//" prefix.
	if prefix == "" || prefix == "/" {
		return mp, nil
	}

	// The account is root inside its daemon, so the mountpoint is untrusted,
	// and path.Join CLEANS: checked on the result, for the reason on Under.
	joined := path.Join(prefix, mp)
	if !Under(prefix, joined, "/") {
		return "", fmt.Errorf(
			"notify: the daemon reported a mountpoint that leaves its own filesystem (%q)", mp)
	}
	return joined, nil
}
