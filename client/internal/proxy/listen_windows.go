//go:build windows

package proxy

import (
	"errors"
	"fmt"
	"net"
	"os/user"
	"strings"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// defaultPipe is where the proxy listens when nothing else is asked for.
const defaultPipe = `\\.\pipe\docker_remote`

// DefaultEndpoint is where the proxy listens when nothing else is asked for.
//
// A function for symmetry with Unix, where the answer is not known until it is
// asked. Here there is a constant behind it.
func DefaultEndpoint() string { return defaultPipe }

// Deliberately NOT \\.\pipe\docker_engine, which is the pipe the official
// Docker CLI looks for when DOCKER_HOST is unset. Binding it would make that
// CLI work with no configuration at all -- but only while nothing else owns
// the name, which in practice means only while Docker Desktop is not running.
// A default that works until the user installs Docker Desktop is worse than
// one that always needs a context, which `workspace create` writes anyway.
//
// This is the same reasoning as the Unix side's note about
// /var/run/docker.sock, for the same reason.

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
		endpoint = defaultPipe
	}

	cfg := &winio.PipeConfig{SecurityDescriptor: ownerOnlySDDL()}

	// Taken for the pid record only; the pipe bind below is what excludes.
	lock, err := acquireLock(endpoint)
	if err != nil {
		return nil, err
	}

	l, err := winio.ListenPipe(endpoint, cfg)
	if err != nil {
		lock.Release()
		// "Access is denied" is what the kernel says when the name is already
		// owned -- accurate, and telling the user nothing they can act on. Any
		// OTHER failure is a real failure and must be reported as itself: a
		// malformed pipe name reported as "already serving" because a stale
		// lock file happened to sit next to it sends the reader hunting for a
		// process that does not exist.
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return nil, &ErrLocked{Endpoint: endpoint, PID: Owner(endpoint)}
		}
		return nil, fmt.Errorf("proxy: listening on %s: %w", endpoint, err)
	}

	// Recorded here, not on acquire: the bind is what decided, so this is the
	// first moment the pid in the file is true.
	lock.writePid()
	return &lockedListener{Listener: l, lock: lock}, nil
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
		endpoint = defaultPipe
	}
	name := strings.TrimPrefix(endpoint, `\\.\pipe\`)
	return "npipe:////./pipe/" + name
}
