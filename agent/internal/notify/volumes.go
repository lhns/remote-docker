package notify

import (
	"context"
	"fmt"
	"path"

	"github.com/lhns/remote-docker/agent/internal/dockercli"
)

// DockerVolumes resolves a volume to its mountpoint through the docker CLI,
// which the workspace image already carries. The agent has no Go Docker
// client, and adding one to ask a single question would be a large dependency
// for a `--format` string -- the same trade internal/server/elevate makes.
type DockerVolumes struct {
	// Host is the daemon to ask, as a -H value. Empty means the agent's own,
	// which is the shared-daemon mode of ADR 0012.
	//
	// With a daemon per account (ADR 0019) the volume being replayed into
	// belongs to that account's daemon and does not exist on any other, so
	// asking the wrong one does not return a wrong path -- it returns no such
	// volume, which is at least loud.
	//
	// A func, and lazy, for the same reason Root is: resolving it eagerly would
	// mean starting the account's daemon when the notify session OPENS rather
	// than when it first replays something, turning the client's connect into a
	// wait for a cold dind. Nil means the agent's own.
	Host func() (string, error)

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
	host, err := call(d.Host)
	if err != nil {
		// Same rule as a root that cannot be resolved, below: refuse rather
		// than fall back. An empty host is the AGENT's daemon, which exists and
		// holds a different set of volumes.
		return "", fmt.Errorf("notify: locating the daemon holding volume %s: %w", volume, err)
	}

	mp, err := dockercli.CLI{Host: host}.Line(ctx,
		"volume", "inspect", volume, "--format", "{{.Mountpoint}}")
	if err != nil {
		return "", fmt.Errorf("notify: inspecting volume %s: %w", volume, err)
	}
	if mp == "" {
		return "", fmt.Errorf("notify: volume %s reported no mountpoint", volume)
	}

	return relocate(mp, d.Root)
}

// call reads a lazily-resolved setting. A nil func is the empty value, which
// both fields document as "the agent's own".
func call(fn func() (string, error)) (string, error) {
	if fn == nil {
		return "", nil
	}
	return fn()
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
	// "" and "/" both mean the identity mapping: the daemon's filesystem IS
	// ours. Only "" used to, and "/" fell through to the join below, where
	// under("/", p, "/") asks whether p starts with "//" and always says no --
	// so every replay on a SHARED daemon was refused as an escape attempt.
	//
	// The integration suite caught that; the unit tests did not, because they
	// asserted the shared target was "/" without ever pushing it through here.
	// Both spellings are accepted now, rather than one being made mandatory: a
	// root of "/" is a true statement, and a resolver is entitled to make it.
	if prefix == "" || prefix == "/" {
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
	if !under(prefix, joined, "/") {
		return "", fmt.Errorf(
			"notify: the daemon reported a mountpoint that leaves its own filesystem (%q)", mp)
	}
	return joined, nil
}
