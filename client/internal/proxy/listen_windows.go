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

// defaultPipe is deliberately not \\.\pipe\docker_engine, which Docker Desktop
// owns whenever it runs; the docker context `remote create` writes finds ours.
const defaultPipe = `\\.\pipe\docker_remote`

// DefaultEndpoint is where the proxy listens when nothing else is asked for.
func DefaultEndpoint() string { return defaultPipe }

// Listen binds the local Docker endpoint as a named pipe, never a loopback
// port: whoever reaches it can mount this machine's files, and only a pipe
// carries an ACL.
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
		// Only "access denied" means the name is owned; anything else is
		// reported as itself.
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return nil, &ErrLocked{Endpoint: endpoint, PID: Owner(endpoint)}
		}
		return nil, fmt.Errorf("proxy: listening on %s: %w", endpoint, err)
	}

	// Only now is the pid true: the bind, not the lock file, decided.
	lock.writePid()
	return &lockedListener{Listener: l, lock: lock}, nil
}

// ownerOnlySDDL grants this user, SYSTEM and Administrators only; the default
// pipe ACL is more generous. Empty (falling back to that default) if the user
// cannot be looked up, rather than refusing to start.
func ownerOnlySDDL() string {
	u, err := user.Current()
	if err != nil || u.Uid == "" {
		return ""
	}
	// Protected DACL: SYSTEM, Administrators, this user; all full access.
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
