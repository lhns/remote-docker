//go:build !windows

package proxy

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
)

// DefaultEndpoint is where the proxy listens when nothing else is asked for.
// Never "": callers append to it for a named workspace, and "" + "-dev" is a
// relative path. Deliberately not /var/run/docker.sock, which is root-owned.
func DefaultEndpoint() string { return defaultSocketPath() }

// Listen binds the local Docker endpoint on a unix socket.
func Listen(endpoint string) (net.Listener, error) {
	if endpoint == "" {
		endpoint = defaultSocketPath()
	}
	if err := os.MkdirAll(filepath.Dir(endpoint), 0o700); err != nil {
		return nil, fmt.Errorf("proxy: creating socket directory: %w", err)
	}

	// Clear the socket only once the lock is held, or this unlinks a running
	// process's socket (ADR 0017).
	lock, err := acquireLock(endpoint)
	if err != nil {
		return nil, err
	}

	if err := os.Remove(endpoint); err != nil && !os.IsNotExist(err) {
		lock.Release()
		return nil, fmt.Errorf("proxy: clearing stale socket: %w", err)
	}

	l, err := net.Listen("unix", endpoint)
	if err != nil {
		lock.Release()
		return nil, fmt.Errorf("proxy: listening on %s: %w", endpoint, err)
	}
	l = &lockedListener{Listener: l, lock: lock}

	// Whoever reaches it can mount this machine's files.
	if err := os.Chmod(endpoint, 0o600); err != nil {
		l.Close()
		return nil, fmt.Errorf("proxy: securing socket: %w", err)
	}
	return l, nil
}

// DockerHost is the DOCKER_HOST value addressing this endpoint.
func DockerHost(endpoint string) string {
	if endpoint == "" {
		endpoint = defaultSocketPath()
	}
	return "unix://" + endpoint
}

func defaultSocketPath() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "remote-docker", "docker.sock")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "remote-docker", "docker.sock")
	}
	return filepath.Join(home, ".local", "state", "remote-docker", "docker.sock")
}
