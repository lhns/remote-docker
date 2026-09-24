//go:build windows

package config

// joinEndpoint names one workspace's pipe beside the default one.
func joinEndpoint(base, name string) string {
	return base + "_" + name
}
