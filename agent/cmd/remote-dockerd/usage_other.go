//go:build !linux

package main

import "errors"

// statfs is unavailable off Linux. The agent only runs on Linux; this exists so
// the command compiles on a development machine.
func statfs(string) (diskUsage, error) {
	return diskUsage{}, errors.New("measuring a disk is only supported on Linux")
}
