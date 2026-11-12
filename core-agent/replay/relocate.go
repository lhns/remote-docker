package replay

import (
	"fmt"
	"path"
)

// Locate splits a mountpoint reported by another daemon at the point where the
// agent stops trusting it.
//
// With a daemon per account the mountpoint is in THAT daemon's filesystem,
// where the account is root and chooses every directory, so the trusted part is
// the daemon's root alone (/proc/<pid>/root) and the whole mountpoint is
// untrusted, to be resolved below it without following a link (nofollow.Open).
// Joining the two into one path would let a link anywhere in it lead out: an
// absolute one resolves against the AGENT's root. With the shared daemon the
// mountpoint is the operator's, so it is the trusted part itself.
//
// `path`, not `path/filepath`: these are Linux paths on both ends, and
// Windows-flavoured joining on the development machine would make the tests lie.
func Locate(mp string, root func() (string, error)) (Mount, error) {
	if root == nil {
		return Mount{Trusted: mp}, nil
	}
	prefix, err := root()
	if err != nil {
		// Never fall back to the unrelocated path: the agent's own
		// /var/lib/docker is the SHARED daemon's, so it exists and would take
		// one account's edits into another daemon's volume.
		return Mount{}, fmt.Errorf("notify: locating the daemon holding the volume: %w", err)
	}
	if prefix == "" || prefix == "/" {
		return Mount{Trusted: mp}, nil
	}

	// Cleaned as a rooted path, which absorbs every "..": none can climb out of
	// the daemon's root, and a link is not looked at here at all.
	return Mount{Trusted: prefix, Path: path.Clean("/" + mp)}, nil
}
