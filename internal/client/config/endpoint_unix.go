//go:build !windows

package config

// endpointSeparator joins the base socket path to a workspace name. A dash
// rather than a path separator, so every workspace's socket sits in the same
// directory and one mkdir covers them all.
const endpointSeparator = "-"
