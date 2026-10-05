//go:build !linux

package accounts

import "os"

// ownerOf cannot tell off Linux, where no account is provisioned anyway.
func ownerOf(os.FileInfo) (int, bool) { return 0, false }

// mountedUnder finds nothing off Linux.
func mountedUnder(string) (string, error) { return "", nil }
