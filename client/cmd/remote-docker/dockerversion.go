package main

// What `docker --version` and `docker version` say about the CLI in here.

import (
	"fmt"
	"runtime/debug"
	"strings"

	dockerversion "github.com/docker/cli/cli/version"
)

// embeddedCLIVersion is the docker/cli version compiled into this binary, from
// the build info: docker/cli sets it by ldflags at its own release, so it would
// otherwise say "unknown-version".
func embeddedCLIVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, dep := range info.Deps {
		if dep.Path != "github.com/docker/cli" {
			continue
		}
		// "v29.7.2+incompatible" -> "29.7.2".
		v := strings.TrimPrefix(dep.Version, "v")
		return strings.TrimSuffix(v, "+incompatible")
	}
	return ""
}

// nameTheEmbeddedCLI sets the version and platform name `docker version` prints.
func nameTheEmbeddedCLI() {
	if v := embeddedCLIVersion(); v != "" {
		dockerversion.Version = v
	}
	dockerversion.PlatformName = "remote-docker " + version
}

// dockerVersionLine is what `docker --version` prints, in docker's shape
// because scripts parse it.
func dockerVersionLine() string {
	return fmt.Sprintf("Docker version %s, build remote-docker %s", orUnknown(embeddedCLIVersion()), version)
}
