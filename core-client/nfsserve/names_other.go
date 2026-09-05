//go:build !windows

package nfsserve

// checkNewName accepts every name: what the host's own filesystem refuses, it
// refuses at the syscall. The Windows rule is in names_windows.go.
func checkNewName(string) error { return nil }
