package union

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"sort"
	"strconv"

	"github.com/lhns/remote-docker/core-agent/netns"
	"github.com/lhns/remote-docker/core/workspace"
)

// How the agent hands a spec to the child it re-executes.
//
// The environment rather than argv, for one reason worth stating: `ps` on a
// workspace shows every account's mounts, and an export path is the one field
// here that names somebody's directory. Neither is a secret, but argv is the
// more public of the two and nothing is gained by putting it there.
const (
	envMode   = "RD_UNION_MODE"
	envPID    = "RD_UNION_PID"
	envExport = "RD_UNION_EXPORT"
	envPort   = "RD_UNION_PORT"
	envCache  = "RD_UNION_CACHE"
	envRead   = "RD_UNION_READ"
)

// What the child is being asked to do.
const (
	// ModeServe prepares the layers and becomes fuse-overlayfs. It does not
	// return.
	ModeServe = "serve"

	// ModeUnmount takes the union down. A separate run; see Release.
	ModeUnmount = "unmount"
)

// envFor encodes a spec, and what to do with it, for the child.
func envFor(mode string, spec Spec) []string {
	return []string{
		envMode + "=" + mode,
		envPID + "=" + strconv.Itoa(spec.PID),
		envExport + "=" + spec.Export,
		envPort + "=" + strconv.Itoa(spec.Port),
		envCache + "=" + spec.CacheDir,
		envRead + "=" + string(spec.Read),
	}
}

// FromEnv reads back what Env wrote, and reports which mode was asked for.
//
// Validated here as well as where it was built. The child runs as root inside
// somebody's daemon, and "the parent checked" is not a property this side can
// see.
func FromEnv(getenv func(string) string) (Spec, string, error) {
	pid, err := strconv.Atoi(getenv(envPID))
	if err != nil {
		return Spec{}, "", fmt.Errorf("union: %s: %w", envPID, err)
	}
	port, err := strconv.Atoi(getenv(envPort))
	if err != nil {
		return Spec{}, "", fmt.Errorf("union: %s: %w", envPort, err)
	}

	spec := Spec{
		PID:      pid,
		Export:   getenv(envExport),
		Port:     port,
		CacheDir: getenv(envCache),
		Read:     workspace.Read(getenv(envRead)),
	}
	if err := spec.Validate(); err != nil {
		return Spec{}, "", err
	}

	mode := getenv(envMode)
	switch mode {
	case ModeServe, ModeUnmount:
		return spec, mode, nil
	default:
		return Spec{}, "", fmt.Errorf("union: %s=%q is not a mode", envMode, mode)
	}
}

// Root is the daemon's filesystem as the agent can read it, which is how the
// agent inspects and writes into a mount it cannot enter. See netns.Root.
func (s Spec) Root() string { return netns.Root(s.PID) }

// errNotAMount is what a path that exists and is not a mount reports. Named
// rather than an os error, because "no such file or directory" about a
// directory that is plainly there is the message this whole check exists to
// stop being given.
var errNotAMount = errors.New("nothing is mounted there")

// Alive reports whether the union answers, and it is the ONLY definition of
// "up" this package offers.
//
// The MOUNT is the truth, not the process: an orphaned server still serves, and
// a live one can hold a mount that answers ENOTCONN. What "mounted" means, and
// why it is not a stat, is on mountedAt. Read through /proc/<pid>/root, which
// resolves in the daemon's namespace without entering it.
//
// A context because a wedged server answers nothing at all: mountedAt's Lstat
// of the merged path blocks on the FUSE server behind it, so it runs on a
// goroutine of its own and every caller asking does not wait with it.
func Alive(ctx context.Context, spec Spec) error {
	merged := path.Join(spec.Root(), spec.Merged())

	done := make(chan error, 1)
	go func() {
		if !mountedAt(merged) {
			done <- errNotAMount
			return
		}
		done <- nil
	}()

	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("union: %s is not serving: %w", spec.Export, err)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("union: %s did not answer: %w", spec.Export, ctx.Err())
	}
}

// MountedShares names the share ids that have a union mounted, reading the
// filesystem under root rather than any process's memory.
//
// Which is the point: after an agent restart the mounts are still serving and
// nothing in this process knows about them. Anything that decides what may be
// deleted has to ask the filesystem, or it will truthfully report "none" about
// unions that are running (ADR 0044).
//
// root is "/" for the shared daemon and /proc/<pid>/root for one per account,
// exactly as Spec.Root gives it.
func MountedShares(root string) []string {
	entries, err := os.ReadDir(path.Join(root, Root))
	if err != nil {
		// No union directory at all is no unions, which is the ordinary case
		// on a workspace that has never served one.
		return nil
	}

	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if mountedAt(path.Join(root, Root, e.Name(), "merged")) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// Reexec is the agent run again as the child that does the work: self is the
// agent's own binary, and mode is what the child is asked to do. The caller
// decides what happens to its output and how long it is supervised.
func Reexec(ctx context.Context, self, mode string, spec Spec) *exec.Cmd {
	cmd := exec.CommandContext(ctx, self, Command)
	cmd.Env = append(os.Environ(), envFor(mode, spec)...)
	return cmd
}

// Unmount takes a union down, through a child; see Release.
func Unmount(ctx context.Context, self string, spec Spec) error {
	if out, err := Reexec(ctx, self, ModeUnmount, spec).CombinedOutput(); err != nil {
		return fmt.Errorf("union: unmounting %s: %w: %s", spec.Export, err, out)
	}
	return nil
}
