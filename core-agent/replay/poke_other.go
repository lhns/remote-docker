//go:build !linux

package replay

import "errors"

// SyscallPoker exercises Linux VFS semantics: which operation produces which
// inotify event. It has no meaning anywhere else, and exists here only so the
// cross-compile matrix stays green.
type SyscallPoker struct{}

func (SyscallPoker) Poke(string, bool) error {
	return errors.New("notify: replaying change events only works on Linux")
}
