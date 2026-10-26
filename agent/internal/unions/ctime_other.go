//go:build !linux

package unions

import "io/fs"

// ctimeOf has nothing to read off Linux, where there are no unions: 0 is not
// known.
func ctimeOf(fs.FileInfo) int64 { return 0 }
