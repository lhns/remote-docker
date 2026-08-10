//go:build windows

package config

// joinEndpoint names one workspace's pipe beside the default one.
//
// Underscore rather than a path separator: everything after \\.\pipe\ is one
// name, and a backslash in it would be a different (nested) namespace. There
// is no extension to preserve, so the name simply goes on the end.
func joinEndpoint(base, name string) string {
	return base + "_" + name
}
