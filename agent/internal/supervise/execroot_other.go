//go:build !linux

package supervise

import "errors"

// The agent runs on Linux. These exist so the module still builds on the
// development machine, and answer the way that leaves the daemon alone.
func mountTmpfs(string) error {
	return errors.New("supervise: a tmpfs can only be mounted on Linux")
}

func mountedAt(string) bool { return false }
