//go:build !windows

package config

import (
	"path/filepath"
	"strings"
)

// joinEndpoint names one workspace's socket beside the default one, keeping
// the extension: docker-dev.sock.
func joinEndpoint(base, name string) string {
	ext := filepath.Ext(base)
	return strings.TrimSuffix(base, ext) + "-" + name + ext
}
