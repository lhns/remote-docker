package union

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"strconv"
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
)

// What the child is being asked to do.
const (
	// ModeServe prepares the layers and becomes fuse-overlayfs. It does not
	// return.
	ModeServe = "serve"

	// ModeUnmount takes the union down. A separate run because umount(2), like
	// mount(2), acts on the caller's own mount namespace, so it has to happen
	// inside the daemon's.
	ModeUnmount = "unmount"
)

// Env encodes a spec for the child.
func Env(spec Spec) []string {
	return envFor(ModeServe, spec)
}

func envFor(mode string, spec Spec) []string {
	return []string{
		envMode + "=" + mode,
		envPID + "=" + strconv.Itoa(spec.PID),
		envExport + "=" + spec.Export,
		envPort + "=" + strconv.Itoa(spec.Port),
		envCache + "=" + spec.CacheDir,
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
// agent inspects and writes into a mount it cannot enter. A zero PID means the
// agent's own filesystem, which is the shared-daemon mode.
func (s Spec) Root() string {
	if s.PID == 0 {
		return "/"
	}
	return fmt.Sprintf("/proc/%d/root", s.PID)
}

// errNotAMount is what a path that exists and is not a mount reports. Named
// rather than an os error, because "no such file or directory" about a
// directory that is plainly there is the message this whole check exists to
// stop being given.
var errNotAMount = errors.New("nothing is mounted there")

// Alive reports whether the union answers, and it is the ONLY definition of
// "up" this package offers.
//
// Deliberately not "is the process running". After an agent restart the server
// is an orphan whose mount still serves every container bound to it; a server
// can also be running with a mount that answers ENOTCONN, which is what killing
// fuse-overlayfs leaves behind. Reading the mountpoint is what tells those
// apart, and it is read through /proc/<pid>/root because that resolves in the
// daemon's mount namespace without entering it.
//
// A context because a wedged server answers nothing at all, and every caller
// asking would otherwise wait with it.
func Alive(ctx context.Context, spec Spec) error {
	merged := path.Join(spec.Root(), spec.Merged())

	done := make(chan error, 1)
	go func() {
		// Whether it is a MOUNT, not whether the path is there. The
		// directories are created before the union is mounted and outlive it,
		// so a stat says yes for a share that never mounted and for one whose
		// server has died -- and the workspace would then declare it ready,
		// let a container bind an ordinary empty directory, and write the
		// cache into it. Nothing would fail; nothing would be a union either.
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

// Unmount takes a union down, through a child, because umount acts on the
// caller's mount namespace.
func Unmount(ctx context.Context, self string, spec Spec) error {
	cmd := exec.CommandContext(ctx, self, Command)
	cmd.Env = append(os.Environ(), envFor(ModeUnmount, spec)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("union: unmounting %s: %w: %s", spec.Export, err, out)
	}
	return nil
}
