//go:build !windows

package proxy

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
)

// DefaultEndpoint is where the proxy listens when nothing else is asked for.
// It is filled in at runtime from the user's runtime directory.
const DefaultEndpoint = ""

// Listen binds the local Docker endpoint on a unix socket.
func Listen(endpoint string) (net.Listener, error) {
	if endpoint == "" {
		endpoint = defaultSocketPath()
	}
	if err := os.MkdirAll(filepath.Dir(endpoint), 0o700); err != nil {
		return nil, fmt.Errorf("proxy: creating socket directory: %w", err)
	}

	// A socket left behind by a process that did not shut down cleanly would
	// otherwise make every later run fail with "address already in use".
	// Removing it is safe because the directory is private to this user.
	if err := os.Remove(endpoint); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("proxy: clearing stale socket: %w", err)
	}

	l, err := net.Listen("unix", endpoint)
	if err != nil {
		return nil, fmt.Errorf("proxy: listening on %s: %w", endpoint, err)
	}

	// Anything able to reach this endpoint can start containers that read and
	// write this machine's filesystem through the NFS export, so it is the
	// owner's alone.
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
