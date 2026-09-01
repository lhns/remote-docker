//go:build linux

package union

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"

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
	if _, err := exec.LookPath(args[0]); err != nil {
		return fmt.Errorf("union: %s is not in the image this daemon runs: %w\n"+
			"\tfix: run the workspace's own image for per-account daemons, with WORKSPACE_DIND_IMAGE", Binary, err)
	}

	// A CHILD rather than an exec, and this is the part no manual states
	// plainly.
	//
	// Entering the mount namespace alone leaves this process holding a pid
	// from the AGENT's pid namespace while looking at the daemon's /proc,
	// which is a procfs for a different one. /proc/self then resolves to
	// nothing, and libfuse reaches for /proc/self/fd constantly. It fails as
	// ENOENT and reports it as "cannot read upper dir: No such file or
	// directory" -- about a directory that is plainly there and that ls in the
	// same shell had just listed (measured, test/union-probe.sh section 11).
	//
	// setns(CLONE_NEWPID) does not move the caller. It decides where its
	// CHILDREN are born, so the union has to be one.
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("union: serving %s: %w", spec.Export, err)
	}
	return nil
}

// enter joins the pid, network and mount namespaces of pid, in that order.
//
// The unshare is not optional and not tidiness: the kernel's mntns_install
// replaces the caller's root and working directory, so it refuses a caller
// whose filesystem state is shared with anything else. Go's threads all share
// it, so it has to be broken first, and it cannot be put back -- which is why
// this happens in a child process rather than in the agent.
func enter(pid int) error {
	// The PID namespace FIRST, and reading its path before the mount namespace
	// changes on purpose: afterwards /proc names the daemon's own procfs
	// rather than the agent's.
	//
	// It moves nothing by itself -- setns(CLONE_NEWPID) decides where this
	// process's CHILDREN are born, which is exactly why the union is a child.
	if err := setns(pid, "pid", unix.CLONE_NEWPID); err != nil {
		return err
	}

	// The NETWORK namespace, and it is the LOWER mount that needs it: with a
	// daemon per account the reverse forward carrying the NFS export is bound
	// inside that daemon's netns and reaches nowhere else (ADR 0019). Mounting
	// from the agent's namespace instead finds nothing on the port, and the
	// kernel reports it as `invalid argument` -- a message about the option
	// string, for a mount that was looking in the wrong network.
	//
	// Before the mount namespace changes, for the same reason the pid one is:
	// afterwards /proc is the daemon's own and this pid names nothing in it.
	if err := setns(pid, "net", unix.CLONE_NEWNET); err != nil {
		return err
	}

	// The unshare is not optional and not tidiness: the kernel's mntns_install
	// replaces the caller's root and working directory, so it refuses a caller
	// whose filesystem state is shared with anything else. Go's threads all
	// share it, so it has to be broken first, and it cannot be put back --
	// which is why this happens in a child process rather than in the agent.
	if err := unix.Unshare(unix.CLONE_FS); err != nil {
		return fmt.Errorf("union: unsharing filesystem state: %w", err)
	}
	return setns(pid, "mnt", unix.CLONE_NEWNS)
}

// setns joins one of pid's namespaces.
func setns(pid int, kind string, flag int) error {
	path := fmt.Sprintf("/proc/%d/ns/%s", pid, kind)
	ns, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("union: opening %s: %w", path, err)
	}
	defer func() { _ = ns.Close() }()

	if err := unix.Setns(int(ns.Fd()), flag); err != nil {
		return fmt.Errorf("union: entering the daemon's %s namespace: %w", kind, err)
	}
	return nil
}

// mountLower mounts the client's export, and treats an existing mount as done.
//
// Idempotent because preparing a share twice is ordinary: a client reconnects,
// a second container wants the same directory. The second call finds the mount
// and says nothing.
func mountLower(spec Spec) error {
	if mountedAt(spec.Lower()) {
		return nil
	}
	source, fstype, options := spec.LowerMount()
	if err := unix.Mount(source, spec.Lower(), fstype, 0, options); err != nil {
		return fmt.Errorf("union: mounting %s at %s: %w", source, spec.Lower(), err)
	}
	return nil
}

// Release enters the daemon's namespace and unmounts the union, then the
// lower. Run in the child, for the same reason the mounting is: umount(2) acts
// on the caller's own mount namespace.
//
// Both unmounts are lazy. A container still holding the mount would otherwise
// make this fail with EBUSY and leave the share half up, and detaching is the
// honest outcome: the share is being released, and whatever still holds it was
// already told the session is over.
func Release(spec Spec) error {
	if err := spec.Validate(); err != nil {
		return err
	}

	runtime.LockOSThread()
	if spec.PID > 0 {
		if err := enter(spec.PID); err != nil {
			return err
		}
	}

	var failed error
	for _, target := range []string{spec.Merged(), spec.Lower()} {
		if !mountedAt(target) {
			continue
		}
		if err := unix.Unmount(target, unix.MNT_DETACH); err != nil {
			failed = fmt.Errorf("union: unmounting %s: %w", target, err)
		}
	}
	return failed
}
