//go:build linux

package union

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"

	"golang.org/x/sys/unix"
)

// Serve is what the re-executed child runs: enter the daemon's namespaces, make
// the three layers, mount the lower, then run fuse-overlayfs. It does not return
// while the union is up.
//
// Why a separate process at all is in this package's doc; why the union is a
// child rather than an exec is at that call. The order matters for one more
// reason: steps 3 and 4 need the daemon's filesystem, which the agent cannot
// see, and fuse-overlayfs is resolved there — which is why the image a daemon
// runs has to carry it (agent/internal/daemons, DefaultImage).
func Serve(spec Spec) error {
	if err := spec.Validate(); err != nil {
		return err
	}

	if err := enterFor(spec); err != nil {
		return err
	}

	for _, dir := range spec.Dirs() {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("union: creating %s: %w", dir, err)
		}
	}
	// The merged root takes the upper's mode, and this process is root; wide,
	// so any uid can create at the top of its share (ADR 0046).
	if err := os.Chmod(spec.Upper(), 0o777); err != nil {
		return fmt.Errorf("union: opening %s to every uid: %w", spec.Upper(), err)
	}

	if err := mountLower(spec); err != nil {
		return err
	}

	args := spec.Args()
	if _, err := exec.LookPath(args[0]); err != nil {
		return fmt.Errorf("union: %s is not in the image this daemon runs: %w\n"+
			"\tfix: run the workspace's own image for per-account daemons, with WORKSPACE_DIND_IMAGE", Binary, err)
	}

	// A child, not an exec. This process holds a pid from the AGENT's pid
	// namespace while looking at the daemon's /proc, so /proc/self resolves to
	// nothing and libfuse reports it as "cannot read upper dir: No such file or
	// directory" about a directory that is there (test/union-probe.sh section
	// 11). setns(CLONE_NEWPID) moves only the caller's CHILDREN.
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("union: serving %s: %w", spec.Export, err)
	}
	return nil
}

// enterFor pins this thread and joins the daemon's namespaces, if the spec
// names one. Pid 0 is the shared daemon, already in this process's own.
//
// Pinned for the rest of this process's life. Nothing unlocks it: the thread's
// filesystem state and namespace are deliberately not the ones the runtime
// handed out, and it is about to be replaced by exec anyway.
func enterFor(spec Spec) error {
	runtime.LockOSThread()
	if spec.PID == 0 {
		return nil
	}
	return enter(spec.PID)
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
	// It moves nothing by itself; the union is the child born after it.
	if err := setns(pid, "pid", unix.CLONE_NEWPID); err != nil {
		return err
	}

	// The NETWORK namespace, and it is the LOWER mount that needs it: with a
	// daemon per account the reverse forward carrying the NFS export is bound
	// inside that daemon's netns and reaches nowhere else (ADR 0019), so a
	// mount attempted from the agent's namespace has no server to talk to.
	//
	// Before the mount namespace changes, for the same reason the pid one is:
	// afterwards /proc is the daemon's own and this pid names nothing in it.
	if err := setns(pid, "net", unix.CLONE_NEWNET); err != nil {
		return err
	}

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
	source, fstype, data, words := spec.LowerMount()
	flags := mountFlagBits(words)
	if err := unix.Mount(source, spec.Lower(), fstype, flags, data); err != nil {
		// The options too: the kernel answers a list it cannot parse with
		// EINVAL, which prints as `invalid argument` and names nothing.
		return fmt.Errorf("union: mounting %s at %s (type %s, flags %v, options %s): %w",
			source, spec.Lower(), fstype, words, data, err)
	}
	return nil
}

// mountFlagBits turns the option words the kernel takes as flags into MS_ bits.
//
// An unknown word is dropped rather than passed on: LowerMount only classes a
// word as a flag if it is one, so anything here that has no bit is a word this
// build does not know, and handing it to the filesystem is what this split
// exists to prevent.
func mountFlagBits(words []string) uintptr {
	bits := map[string]uintptr{
		"noatime":     unix.MS_NOATIME,
		"relatime":    unix.MS_RELATIME,
		"strictatime": unix.MS_STRICTATIME,
		"ro":          unix.MS_RDONLY,
		"nosuid":      unix.MS_NOSUID,
		"nodev":       unix.MS_NODEV,
		"noexec":      unix.MS_NOEXEC,
		"sync":        unix.MS_SYNCHRONOUS,
		"dirsync":     unix.MS_DIRSYNC,
	}

	var flags uintptr
	for _, w := range words {
		flags |= bits[w]
	}
	return flags
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

	if err := enterFor(spec); err != nil {
		return err
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
