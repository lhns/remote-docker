//go:build windows

package config

// endpointSeparator joins the base pipe name to a workspace name. Underscore
// rather than a path separator: everything after \.\pipe\ is one name, and a
// backslash in it would be a different (nested) namespace.
const endpointSeparator = "_"
