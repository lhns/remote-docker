// Package mount manages the workspace's convenience mount at ~/workspace.
//
// This is all that remains of the mount helpers and the sudoers file. Since
// bind mounts became per-container volumes (ADR 0006), containers no longer
// need anything mounted in the workspace's own namespace -- the daemon mounts
// each volume for itself. What is left is somewhere for an interactive shell
// to land, which is worth having and worth nothing more than that.
package mount

import (
	"errors"
	"fmt"
	"io/fs"
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

	// EEXIST is not a failure: something is already at that name, which is
	// all a mountpoint has to be.
	//
	// It happens for a reason worth naming. If a previous client died without
	// unmounting -- Ctrl-C, a dropped link, a laptop lid -- the mount is left
	// stale, and stat on a stale NFS mount fails rather than reporting a
	// directory. IsMounted therefore says "not mounted", MkdirAll falls
	// through to mkdir, and mkdir says EEXIST. Treating that as fatal left the
	// user with a permanently broken ~/workspace and no way to fix it from the
	// client, which is how this was found.
	if err := os.MkdirAll(mountpoint, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("mount: creating %s: %w", mountpoint, err)
	}
	// Ownership of the mountpoint itself only matters while nothing is mounted
	// on it; once the NFS mount is up, attributes come from the server. So a
	// failure here -- again, likely a stale mount -- must not stop the mount
	// that would fix it.
	if err := os.Chown(mountpoint, uid, gid); err != nil {
		m.logf("could not set ownership of %s: %v", mountpoint, err)
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
		// One retry, after forcing off whatever was there. A stale mount from
		// a previous session is the common cause and it cannot clear itself:
		// every later attempt hits the same corpse. Lazy, because a stale
		// NFS mount cannot be unmounted any other way while anything still
		// holds a reference into it.
		if m.forceUnmount(mountpoint) {
			cmd = exec.Command("mount", "-t", "nfs",
				"127.0.0.1:"+workspace.ExportCWD, mountpoint,
				"-o", opts["o"])
			if out2, err2 := cmd.CombinedOutput(); err2 == nil {
				m.mounted[mountpoint] = true
				return nil
			} else {
				out, err = out2, err2
			}
		}
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

// forceUnmount clears whatever is at a mountpoint, reporting whether it is
// worth trying to mount again.
//
// Lazy unmount is the only thing that works on a stale NFS mount: a plain
// umount needs to talk to a server that is not answering, which is what made
// it stale.
func (m *Manager) forceUnmount(mountpoint string) bool {
	if out, err := exec.Command("umount", "-l", mountpoint).CombinedOutput(); err != nil {
		m.logf("could not clear %s before remounting: %v: %s",
			mountpoint, err, strings.TrimSpace(string(out)))
		return false
	}
	m.logf("cleared a stale mount at %s", mountpoint)
	return true
}

func (m *Manager) logf(format string, args ...any) {
	if m.Log != nil {
		m.Log.Printf(format, args...)
	}
}
