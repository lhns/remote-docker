//go:build windows

package proxy

import (
	"fmt"
	"net"
	"os/user"
	"strings"

	"github.com/Microsoft/go-winio"
)

// DefaultEndpoint is where the proxy listens when nothing else is asked for.
const DefaultEndpoint = `\\.\pipe\remote-docker`

// DockerEngineEndpoint is the pipe the official Docker CLI looks for when
// DOCKER_HOST is unset. Binding it makes the official CLI work with no
// configuration at all -- but only when nothing else already owns the name,
// which in practice means Docker Desktop is not running.
const DockerEngineEndpoint = `\\.\pipe\docker_engine`

// Listen binds the local Docker endpoint.
//
// A named pipe rather than a TCP port on loopback, and that is a security
// decision rather than a stylistic one: anything able to reach this endpoint
// can start a container that reads and writes this machine's filesystem
// through the NFS export. A loopback port is reachable by every process and
// every user on the machine with no way to restrict it; a pipe carries an ACL.
//
// Creating a pipe under \\.\pipe\ needs no administrator rights.
func Listen(endpoint string) (net.Listener, error) {
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}

	cfg := &winio.PipeConfig{SecurityDescriptor: ownerOnlySDDL()}

	l, err := winio.ListenPipe(endpoint, cfg)
	if err != nil {
		return nil, fmt.Errorf("proxy: listening on %s: %w", endpoint, err)
	}
	return l, nil
}

// ownerOnlySDDL grants this user, SYSTEM and Administrators, and nobody else.
//
// The alternative -- passing a nil config -- takes the Windows default named
// pipe ACL from RtlDefaultNpAcl, which is more generous than is appropriate
// for an endpoint that can mount this machine's filesystem into a container.
// Being explicit costs one lookup and removes the question.
//
// An empty descriptor falls back to that default rather than failing to
// listen: a client that will not start is worse than one whose pipe is as
// permissive as every other named pipe on the system, and the caller can
// still choose a different endpoint.
func ownerOnlySDDL() string {
	u, err := user.Current()
	if err != nil || u.Uid == "" {
		return ""
	}
	// D:P            a protected DACL, so nothing is inherited
	// (A;;GA;;;SY)   Local System, full
	// (A;;GA;;;BA)   Builtin Administrators, full
	// (A;;GA;;;<sid>) this user, full
	var b strings.Builder
	b.WriteString("D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;")
	b.WriteString(u.Uid)
	b.WriteString(")")
	return b.String()
}

// DockerHost is the DOCKER_HOST value addressing this endpoint.
func DockerHost(endpoint string) string {
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	name := strings.TrimPrefix(endpoint, `\\.\pipe\`)
	return "npipe:////./pipe/" + name
}
