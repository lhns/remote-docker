package main

// The name this binary was installed as.
//
// It is the Docker CLI at the root, so renaming the file to `docker` is a
// complete installation and needs no code. What is left is cosmetic and
// worth getting right: help and error messages should use the name the reader
// typed, not one this program chose for itself.

import (
	"path/filepath"
	"strings"
)

// programName is the file this binary was installed as, without its extension.
//
// selfPath rather than os.Args[0]: an exec wrapper can leave the loader there
// (see self.go), and "linker64: no such command" would name the wrong program
// entirely. The file on disk is what the user renamed, which is the question
// being asked.
func programName() string {
	self, err := selfPath()
	if err != nil {
		return defaultName
	}
	name := filepath.Base(self)
	name = strings.TrimSuffix(name, filepath.Ext(name))
	if name == "" || name == "." {
		return defaultName
	}
	return name
}

// defaultName is what to call ourselves when the file cannot be found at all.
const defaultName = "remote-docker"

// ours renders one of this program's own commands the way the reader would
// type it: under the name their copy of the binary has, and under `remote`.
//
// Never a literal "remote-docker ..." in a message. The file may be named
// `docker`, which is the documented installation, and a fix line naming a
// program the reader does not have is worse than no fix line.
func ours(command string) string {
	return programName() + " remote " + command
}
