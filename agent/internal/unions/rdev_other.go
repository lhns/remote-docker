//go:build !linux

package unions

import "io/fs"

// rdev has no meaning off Linux, where there are no overlay whiteouts to find.
func rdev(fs.FileInfo) uint64 { return 1 }
