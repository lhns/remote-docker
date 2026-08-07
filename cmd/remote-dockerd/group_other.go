//go:build !linux

package main

// addGroup is a no-op off Linux. The agent only ever runs in a Linux
// container; this exists so the command compiles on a development machine.
func addGroup(string) error { return nil }
