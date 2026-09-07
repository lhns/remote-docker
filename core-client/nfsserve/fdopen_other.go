//go:build !windows

package nfsserve

import "os"

// openShared is an ordinary open everywhere but Windows: an open descriptor
// never stopped anything unlinking or renaming the file.
func openShared(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR, 0)
}
