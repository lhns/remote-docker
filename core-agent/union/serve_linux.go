//go:build linux

package union

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"syscall"

	"golang.org/x/sys/unix"
)

// Serve is what the re-executed child runs. It enters the daemon's mount
// namespace, prepares the three layers, and becomes fuse-overlayfs.
//
// It never returns on success, because the last thing it does is exec.
//
// The order is the whole of it, and each step is there for a measured reason:
//
//  1. unshare the filesystem state, because setns(CLONE_NEWNS) refuses a caller
//     that shares it and every Go thread does. This also makes the following
//     step's replacement of root and cwd private to this thread.
//  2. enter the namespace, which sets root and cwd to the daemon's. From here
//     an absolute path means what it means inside the daemon.
//  3. make the directories and mount the lower. Done here rather than by the
//     agent because the agent cannot see these paths at all.
//  4. exec fuse-overlayfs, which mounts the union itself. It is resolved in
//     the daemon's filesystem, which is why the image the daemon runs has to
//     carry it (agent/internal/daemons/plan.go:38).
func Serve(spec Spec) error {
	if err := spec.Validate(); err != nil {
		return err
	}

	// Pinned for the rest of this process's life. Nothing unlocks it: the
	// thread's filesystem state and namespace are deliberately not the ones
	// the runtime handed out, and it is about to be replaced by exec anyway.
	runtime.LockOSThread()

	if spec.PID > 0 {
		if err := enter(spec.PID); err != nil {
			return err
		}
	}

	for _, dir := range spec.Dirs() {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("union: creating %s: %w", dir, err)
		}
	}

	if err := mountLower(spec); err != nil {
		return err
	}

	args := spec.Args()
	binary, err := exec.LookPath(args[0])
	if err != nil {
		return fmt.Errorf("union: %s is not in the image this daemon runs: %w\n"+
			"\tfix: run the workspace's own image for per-account daemons, with WORKSPACE_DIND_IMAGE", Binary, err)
	}
	return syscall.Exec(binary, args, os.Environ())
}

// enter joins the mount namespace of pid.
//
// The unshare is not optional and not tidiness: the kernel's mntns_install
// replaces the caller's root and working directory, so it refuses a caller
// whose filesystem state is shared with anything else. Go's threads all share
// it, so it has to be broken first, and it cannot be put back -- which is why
// this happens in a child process rather than in the agent.
func enter(pid int) error {
	if err := unix.Unshare(unix.CLONE_FS); err != nil {
		return fmt.Errorf("union: unsharing filesystem state: %w", err)
	}

	path := fmt.Sprintf("/proc/%d/ns/mnt", pid)
	ns, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("union: opening %s: %w", path, err)
	}
	defer func() { _ = ns.Close() }()

	if err := unix.Setns(int(ns.Fd()), unix.CLONE_NEWNS); err != nil {
		return fmt.Errorf("union: entering the daemon's mount namespace: %w", err)
	}
	return nil
}

// mountLower mounts the client's export, and treats an existing mount as done.
//
// Idempotent because preparing a share twice is ordinary: a client reconnects,
// a second container wants the same directory. The second call finds the mount
// and says nothing.
func mountLower(spec Spec) error {
	if mounted(spec.Lower()) {
		return nil
	}
	source, fstype, options := spec.LowerMount()
	if err := unix.Mount(source, spec.Lower(), fstype, 0, options); err != nil {
		return fmt.Errorf("union: mounting %s at %s: %w", source, spec.Lower(), err)
	}
	return nil
}

// mounted reports whether anything is mounted at path, by asking whether the
// path and its parent are on the same device.
//
// Cheaper and more direct than parsing /proc/self/mountinfo, and it is asked
// from inside the namespace that owns the mount, which is the only place the
// answer means anything.
func mounted(path string) bool {
	var here, up unix.Stat_t
	if err := unix.Lstat(path, &here); err != nil {
		return false
	}
	if err := unix.Lstat(path+"/..", &up); err != nil {
		return false
	}
	return here.Dev != up.Dev
}
