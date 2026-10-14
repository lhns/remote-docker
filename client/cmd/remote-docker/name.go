package main

import (
	"path/filepath"
	"strings"

	"github.com/lhns/remote-docker/client/internal/session"
)

// programName is the file this binary was installed as (often `docker`),
// without its extension. selfPath, not os.Args[0], which may be the loader (self.go).
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

// ourCommand renders one of our commands as the reader would type it. Use it
// instead of a literal "remote-docker ..." in any message.
func ourCommand(command string) string {
	return programName() + " remote " + command
}

// The session's messages name commands too, and must spell them the same way.
func init() { session.OurCommand = ourCommand }

// version is set at build time; a client compares it with its session's.
var version = "dev"
