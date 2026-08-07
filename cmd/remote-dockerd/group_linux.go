//go:build linux

package main

import (
	"os/exec"
	"os/user"
)

// addGroup creates a system group if it is missing.
//
// Shelling out to addgroup rather than editing /etc/group: the alpine and
// shadow tools handle the locking between the group and gshadow files, and
// reimplementing that to save a dependency would trade a well-understood tool
// for a novel source of corruption.
func addGroup(name string) error {
	if _, err := user.LookupGroup(name); err == nil {
		return nil
	}
	// -S is alpine's system-group flag; groupadd -r is the shadow spelling.
	// The image has one or the other, so try both and only report if neither
	// worked.
	if err := exec.Command("addgroup", "-S", name).Run(); err == nil {
		return nil
	}
	return exec.Command("groupadd", "-r", name).Run()
}
