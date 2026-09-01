//go:build !linux

package union

import "os"

// mountedAt cannot be answered off Linux, where none of this runs: a union is
// mounted inside a workspace daemon. Existing is the most it can say, and it
// exists so the package still builds on the development machine.
func mountedAt(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
