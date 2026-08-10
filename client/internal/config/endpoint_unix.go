//go:build !windows

package config

import (
	"path/filepath"
	"strings"
)

// joinEndpoint names one workspace's socket beside the default one.
//
// The name goes BEFORE the extension -- `docker-dev.sock`, not
// `docker.sock-dev`, because a unix socket path is a filename and tools that
// glob for `*.sock` are ordinary. A dash rather than a subdirectory, so every
// workspace's socket sits in the same directory and one mkdir covers them all.
func joinEndpoint(base, name string) string {
	ext := filepath.Ext(base)
	return strings.TrimSuffix(base, ext) + "-" + name + ext
}
