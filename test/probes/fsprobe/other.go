//go:build !linux

package main

import (
	"errors"
	"io"
)

// The probe performs raw Linux VFS syscalls and only means anything there.
// These stubs keep `go build ./...` green on every other platform.
func run(string, []string, io.Writer) error {
	return errors.New("fsprobe exercises Linux filesystem semantics and only runs on Linux")
}

func childMain(string, []string) int {
	return 1
}
