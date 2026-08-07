// Package mount manages the workspace's convenience mount at ~/workspace.
//
// This is all that remains of the mount helpers and the sudoers file. Since
// bind mounts became per-container volumes (ADR 0006), containers no longer
// need anything mounted in the workspace's own namespace -- the daemon mounts
// each volume for itself. What is left is somewhere for an interactive shell
// to land, which is worth having and worth nothing more than that.
package mount

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/lhns/remote-docker/pkg/workspace"
)

// Logger reports mounts and failures.
type Logger interface {
	Printf(format string, args ...any)
}

// Manager mounts and unmounts workspace directories.
type Manager struct {
	Log Logger

	mu      sync.Mutex
	mounted map[string]bool
}

// New returns a manager.
func New(log Logger) *Manager {
	return &Manager{Log: log, mounted: map[string]bool{}}
}

// Ensure mounts the client's export at the account's ~/workspace if it is not
// already there.
//
// Called when an interactive session starts rather than when the reverse
// forward is established, and that ordering is deliberate: the forward exists
// as soon as the client asks for it, but the client's NFS server begins
// serving a moment later. Mounting on the forward would race it and fail for
// no reason the user could act on.
func (m *Manager) Ensure(home string, uid, gid, port int) error {
	if home == "" {
		return fmt.Errorf("mount: the account has no home directory")
	}
	mountpoint := filepath.Join(home, "workspace")

	m.mu.Lock()
	defer m.mu.Unlock()

	if IsMounted(mountpoint) {
		return nil
	}

	if err := os.MkdirAll(mountpoint, 0o755); err != nil {
		return fmt.Errorf("mount: creating %s: %w", mountpoint, err)
	}
	if err := os.Chown(mountpoint, uid, gid); err != nil {
		return fmt.Errorf("mount: owning %s: %w", mountpoint, err)
	}

	// The same options the client puts on its volumes, from the same place, so
	// the shell's view and a container's view cannot drift apart. soft plus a
	// short timeo means a dead tunnel fails I/O rather than parking the shell
	// in uninterruptible sleep.
	opts := workspace.NFSVolumeOptions(port, workspace.ExportCWD)

	cmd := exec.Command("mount", "-t", "nfs",
		"127.0.0.1:"+workspace.ExportCWD, mountpoint,
		"-o", opts["o"])
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mount: %w: %s", err, strings.TrimSpace(string(out)))
	}

	m.mounted[mountpoint] = true
	m.logf("mounted 127.0.0.1:%d at %s", port, mountpoint)
	return nil
}

// Release unmounts an account's workspace, if we mounted it.
func (m *Manager) Release(home string) {
	if home == "" {
		return
	}
	mountpoint := filepath.Join(home, "workspace")

	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.mounted[mountpoint] {
		return
	}
	delete(m.mounted, mountpoint)

	if !IsMounted(mountpoint) {
		return
	}

	// Lazy as the fallback: something may still hold it, and a lazy unmount
	// detaches it now and cleans up when the last user goes. Leaving a stale
	// NFS mount behind is worse -- the next session would find a mountpoint
	// pointing at a dead tunnel and consider it already mounted.
	if out, err := exec.Command("umount", mountpoint).CombinedOutput(); err != nil {
		if out2, err2 := exec.Command("umount", "-l", mountpoint).CombinedOutput(); err2 != nil {
			m.logf("could not unmount %s: %s / %s", mountpoint,
				strings.TrimSpace(string(out)), strings.TrimSpace(string(out2)))
			return
		}
	}
	m.logf("unmounted %s", mountpoint)
}

func (m *Manager) logf(format string, args ...any) {
	if m.Log != nil {
		m.Log.Printf(format, args...)
	}
}
