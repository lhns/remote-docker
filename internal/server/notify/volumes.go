package notify

import (
	"context"
	"fmt"
	"os/exec"
	"path"
	"strings"
)

// DockerVolumes resolves a volume to its mountpoint through the docker CLI,
// which the workspace image already carries. The agent has no Go Docker
// client, and adding one to ask a single question would be a large dependency
// for a `--format` string -- the same trade internal/server/elevate makes.
type DockerVolumes struct {
	// Docker is the binary to run. Empty means "docker" from PATH.
	Docker string

	// Host is the daemon to ask, as a -H value. Empty means the agent's own,
	// which is the shared-daemon mode of ADR 0012.
	//
	// With a daemon per account (ADR 0019) the volume being replayed into
	// belongs to that account's daemon and does not exist on any other, so
	// asking the wrong one does not return a wrong path -- it returns no such
	// volume, which is at least loud.
	Host string

	// Root maps that daemon's filesystem into ours.
	//
	// A per-account daemon reports a mountpoint in ITS OWN filesystem, which
	// the agent cannot open by that path. /proc/<pid>/root is how the agent
	// reaches it, and this promotes that route from the fallback ADR 0016
	// measured to load-bearing.
	//
	// A func rather than a string because the pid changes every time the
	// daemon restarts, and mountpoints are re-resolved often enough for a
	// captured one to go stale. Nil means the mountpoint is already ours.
	Root func() (string, error)
}

func (d DockerVolumes) Mountpoint(ctx context.Context, volume string) (string, error) {
	bin := d.Docker
	if bin == "" {
		bin = "docker"
	}

	args := []string{"volume", "inspect", volume, "--format", "{{.Mountpoint}}"}
	if d.Host != "" {
		args = append([]string{"-H", d.Host}, args...)
	}

	out, err := exec.CommandContext(ctx, bin, args...).Output()
	if err != nil {
		return "", fmt.Errorf("notify: inspecting volume %s: %w", volume, err)
	}
	mp := strings.TrimSpace(string(out))
	if mp == "" {
		return "", fmt.Errorf("notify: volume %s reported no mountpoint", volume)
	}

	return relocate(mp, d.Root)
}

// relocate maps a mountpoint reported by another daemon into our filesystem.
//
// Separated from the exec above so it can be tested without one -- unit tests
// here must run with no daemon and on a machine that is not Linux (CLAUDE.md).
//
// `path`, not `path/filepath`: these are always Linux paths, produced by a
// Linux daemon and consumed by a Linux agent, and running them through
// Windows-flavoured joining on the development machine would only make the
// test lie about what production does.
func relocate(mp string, root func() (string, error)) (string, error) {
	if root == nil {
		return mp, nil
	}
	prefix, err := root()
	if err != nil {
		// Never fall back to the unrelocated path. The agent's own
		// /var/lib/docker is the SHARED daemon's, so that path exists and
		// means something else -- a silent fallback would replay one account's
		// edits into another daemon's volume.
		return "", fmt.Errorf("notify: locating the daemon holding the volume: %w", err)
	}
	if prefix == "" {
		return mp, nil
	}

	// Joining is not containment, and assuming it was is a mistake this very
	// test caught: path.Join CLEANS, so joining "/proc/42/root" to
	// "/../../etc/shadow" yields "/proc/etc/shadow" -- outside the root, with
	// no error, having looked correct.
	//
	// That matters here and did not before. The mountpoint is whatever the
	// account's daemon says it is, and in per-account mode the account is root
	// inside that daemon's container: this is attacker-controlled input to a
	// root process deciding which path to touch. So the result is checked
	// rather than trusted.
	joined := path.Join(prefix, mp)
	if joined != prefix && !strings.HasPrefix(joined, prefix+"/") {
		return "", fmt.Errorf(
			"notify: the daemon reported a mountpoint that leaves its own filesystem (%q)", mp)
	}
	return joined, nil
}
