//go:build windows

package daemons

// lookupIDs exists so the package builds on the development machine, which has
// no Docker and no Linux by the premise of this project (see CLAUDE.md). The
// agent runs on Linux only; nothing here is a portability claim.
func lookupIDs(string) (int, int, error) { return 0, 0, ErrUnsupported }
