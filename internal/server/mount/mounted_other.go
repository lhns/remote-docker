//go:build !linux

package mount

// IsMounted is Linux-only, like the agent that uses it. This exists so the
// package compiles on a development machine.
func IsMounted(string) bool { return false }
