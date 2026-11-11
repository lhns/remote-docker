//go:build !linux

package unions

import "os"

// opaqueDir has no meaning off Linux, where there are no overlays.
func opaqueDir(*os.Root, string) bool { return false }
