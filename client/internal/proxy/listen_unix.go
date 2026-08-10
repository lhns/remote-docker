//go:build !windows

package proxy

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
)

// DefaultEndpoint is where the proxy listens when nothing else is asked for.
//
// A function rather than a constant, because on this platform the answer is
// not known until it is asked: it comes from the user's runtime directory.
// It used to be the empty string, resolved inside Listen, which was fine
// for Listen and wrong for everybody else. A caller deriving a NAMED
// workspace's endpoint appends to this, and appending to "" produced the
// relative path "-dev": a socket in whatever directory the process happened
// to be in, and a docker context pointing at unix://-dev.
func DefaultEndpoint() string { return defaultSocketPath() }

// Deliberately NOT /var/run/docker.sock, the path the official CLI uses by
// default. It is root-owned, so serving it would need privileges this client
// otherwise never asks for, and it would collide with any local daemon. The
// docker context written by `remote-docker context install` needs none and
// behaves the same on every platform.

// Listen binds the local Docker endpoint on a unix socket.
func Listen(endpoint string) (net.Listener, error) {
	if endpoint == "" {
		endpoint = defaultSocketPath()
	}
	if err := os.MkdirAll(filepath.Dir(endpoint), 0o700); err != nil {
		return nil, fmt.Errorf("proxy: creating socket directory: %w", err)
	}

	// The lock is what earns the right to clear the socket.
	//
	// A socket left by a process that did not shut down cleanly must be
	// removed, or every later run fails with "address already in use". But
	// removing it unconditionally (which is what this did) silently
	// unlinks a RUNNING process's socket and takes its place: the first keeps
	// accepting on an inode nobody can reach, and when the second exits the
	// path is bound to nothing while the first still looks healthy. Holding
	// the lock means the only socket we can be clearing is a dead one.
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
