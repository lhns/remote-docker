//go:build !linux

package main

import (
	"errors"
	"io"
)

// The probe performs raw Linux VFS syscalls and only means anything there.
// These stubs exist so `go build ./...` stays green on the development
// machine, which has neither Docker nor a Linux kernel.
func run(string, []string, bool, io.Writer) error {
	return errors.New("fsprobe exercises Linux filesystem semantics and only runs on Linux")
}

func childMain(string, []string) int {
	return 1
}
