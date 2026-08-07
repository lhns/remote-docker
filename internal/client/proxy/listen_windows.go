//go:build windows

package proxy

import (
	"fmt"
	"net"
	"strings"

	"github.com/Microsoft/go-winio"
)

// DefaultEndpoint is where the proxy listens when nothing else is asked for.
const DefaultEndpoint = `\\.\pipe\remote-docker`

// Listen binds the local Docker endpoint.
//
// A named pipe rather than an AF_UNIX socket: both exist on modern Windows,
// but npipe:// is the transport every Windows Docker client already speaks, so
// pointing DOCKER_HOST at this needs no special support from anything.
//
// The nil config leaves the pipe's default security descriptor in place, which
// grants the creating user and administrators and nobody else. That matters
// more here than it looks: anything able to reach this endpoint can start
// containers that read and write this machine's filesystem through the NFS
// export.
func Listen(endpoint string) (net.Listener, error) {
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	l, err := winio.ListenPipe(endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("proxy: listening on %s: %w", endpoint, err)
	}
	return l, nil
}

// DockerHost is the DOCKER_HOST value addressing this endpoint.
func DockerHost(endpoint string) string {
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	name := strings.TrimPrefix(endpoint, `\\.\pipe\`)
	return "npipe:////./pipe/" + name
}
