package replay

// Mapping a path reported by another daemon into this filesystem.
//
// Kept beside the replayer rather than beside the thing that asks a daemon,
// because what it enforces is containment and containment is the replayer's
// promise: the paths it is handed are attacker-controlled and it performs
// syscalls on the user's own files.

import (
	"fmt"
	"path"
)

// Relocate maps a mountpoint reported by another daemon into our filesystem.
//
// Separated from the exec above so it can be tested without one: the tests
// here run with no daemon, on a machine that is not Linux.
//
// `path`, not `path/filepath`: these are always Linux paths, produced by a
// Linux daemon and consumed by a Linux agent, and running them through
// Windows-flavoured joining on the development machine would only make the
// test lie about what production does.
func Relocate(mp string, root func() (string, error)) (string, error) {
	if root == nil {
		return mp, nil
	}
	prefix, err := root()
	if err != nil {
		// Never fall back to the unrelocated path. The agent's own
		// /var/lib/docker is the SHARED daemon's, so that path exists and
		// means something else. A silent fallback would replay one account's
		// edits into another daemon's volume.
		return "", fmt.Errorf("notify: locating the daemon holding the volume: %w", err)
	}
	// "" and "/" both mean the identity mapping: the daemon's filesystem IS
	// ours. Accept both rather than making one mandatory -- a root of "/" is a
	// true statement and a resolver is entitled to make it.
	//
	// Letting "/" fall through to the join below does not merely look wrong, it
	// refuses everything: Under("/", p, "/") asks whether p starts with "//",
	// which is never true, so every replay on a shared daemon reads as an
	// escape attempt.
	if prefix == "" || prefix == "/" {
		return mp, nil
	}

	// The mountpoint is whatever the account's daemon says it is, and in
	// per-account mode the account is root inside that daemon's container:
	// attacker-controlled input to a root process deciding which path to
	// touch. Checked on the result, for the reason on Under.
	joined := path.Join(prefix, mp)
	if !Under(prefix, joined, "/") {
		return "", fmt.Errorf(
			"notify: the daemon reported a mountpoint that leaves its own filesystem (%q)", mp)
	}
	return joined, nil
}
