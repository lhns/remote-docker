//go:build !linux

package main

import "errors"

// The primitives are Linux VFS semantics and only mean anything there. This
// stub exists so `go build ./...` and the cross-compile matrix stay green on
// the development machine, which has neither Docker nor a Linux kernel.
func poke(string, string) (string, error) {
	return "", errors.New("pokeprobe exercises Linux VFS semantics and only runs on Linux")
}
