package main

// What `docker --version` and `docker version` say about the CLI in here.

import (
	"fmt"
	"runtime/debug"
	"strings"

	dockerversion "github.com/docker/cli/cli/version"
)

// embeddedCLIVersion is the docker/cli version compiled into this binary.
//
// Read from the build info rather than written down. docker/cli sets its own
// version through ldflags at ITS release, which we do not perform, so
// without this the embedded CLI reports "unknown-version", and `docker
// version` on a machine with no other docker is the only place to find out
// what is actually running.
//
// A constant would drift: the version lives in go.mod and dependabot moves it.
func embeddedCLIVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, dep := range info.Deps {
		if dep.Path != "github.com/docker/cli" {
			continue
		}
		// "v29.7.2+incompatible" is how a pre-modules major version arrives.
		// Neither part means anything to somebody reading a version number.
		v := strings.TrimPrefix(dep.Version, "v")
		return strings.TrimSuffix(v, "+incompatible")
	}
	return ""
}

// nameTheEmbeddedCLI tells docker/cli what it is, so its own `version` command
// stops saying "unknown-version" and says who is carrying it.
//
// PlatformName is the line docker prints as "Client: Docker Engine -
// Community". Ours is not that, and saying so tells somebody reading `docker
// version` which program they are holding.
func nameTheEmbeddedCLI() {
	if v := embeddedCLIVersion(); v != "" {
		dockerversion.Version = v
	}
	dockerversion.PlatformName = "remote-docker " + version
}

// dockerVersionLine is what `docker --version` prints.
//
// The shape is docker's ("Docker version X, build Y") because that is what
// scripts and tools parse, and a different shape here would break them for no
// gain. The build field says remote-docker rather than a commit we do not
// have, which is the honest thing to put in it and also the useful one.
func dockerVersionLine() string {
	v := embeddedCLIVersion()
	if v == "" {
		v = "unknown"
	}
	return fmt.Sprintf("Docker version %s, build remote-docker %s", v, version)
}
