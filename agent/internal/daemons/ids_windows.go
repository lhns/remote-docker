//go:build windows

package daemons

// lookupIDs exists so the package builds on the development machine. The agent
// runs on Linux only; this is not a portability claim.
func lookupIDs(string) (int, int, error) { return 0, 0, ErrUnsupported }
