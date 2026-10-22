//go:build !linux

package unions

// opaqueDir has no meaning off Linux, where there are no overlays.
func opaqueDir(string) bool { return false }
