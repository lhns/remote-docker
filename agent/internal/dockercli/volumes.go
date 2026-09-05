package dockercli

import (
	"context"
	"fmt"
	"strings"

	"github.com/lhns/remote-docker/core-agent/replay"
)

// Volumes resolves a managed volume to its mountpoint through the docker CLI,
// which the workspace image already carries. The agent has no Go Docker client,
// and adding one to ask a single question would be a large dependency for one
// `--format` string. The same trade elevate makes.
//
// It lives here rather than beside the replayer because it is the only part of
// replaying that knows Docker exists. What the replayer needs is a name to a
// directory, which replay.Volumes states and this answers.
type Volumes struct {
	// Host is the daemon to ask, as a -H value. Empty means the agent's own,
	// which is the shared-daemon mode of ADR 0012.
	//
	// With a daemon per account (ADR 0019) the volume being replayed into
	// belongs to that account's daemon and does not exist on any other, so
	// asking the wrong one does not return a wrong path. It returns no such
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

func (v Volumes) Mountpoint(ctx context.Context, volume string) (string, error) {
	host, err := call(v.Host)
	if err != nil {
		// Same rule as a root that cannot be resolved: refuse rather than fall
		// back. An empty host is the AGENT's daemon, which exists and holds a
		// different set of volumes.
		return "", fmt.Errorf("notify: locating the daemon holding volume %s: %w", volume, err)
	}

	mp, err := mountpoint(ctx, host, volume)
	if err != nil {
		return "", err
	}
	return replay.Relocate(mp, v.Root)
}

// mountpoint asks the daemon at host where a volume's data is, in the daemon's
// own filesystem.
func mountpoint(ctx context.Context, host, volume string) (string, error) {
	mp, err := CLI{Host: host}.Line(ctx, "volume", "inspect", volume, "--format", "{{.Mountpoint}}")
	if err != nil {
		return "", fmt.Errorf("dockercli: inspecting volume %s: %w", volume, err)
	}
	if mp == "" {
		return "", fmt.Errorf("dockercli: volume %s reported no mountpoint", volume)
	}
	return mp, nil
}

// call reads a lazily-resolved setting. A nil func is the empty value, which
// both fields document as "the agent's own".
func call(fn func() (string, error)) (string, error) {
	if fn == nil {
		return "", nil
	}
	return fn()
}

// RawVolumes answers where a volume's data lives INSIDE the daemon's own
// filesystem, without relocating it into the agent's.
//
// The distinction matters for exactly one caller. A union is mounted in the
// daemon's mount namespace (ADR 0044), so the layer paths it is given have to
// mean something THERE; a path relocated through /proc/<pid>/root names nothing
// inside that namespace. Everything else wants Volumes, which relocates,
// because everything else reads the files from out here.
type RawVolumes struct{}

// RawMountpoint asks the daemon at host where a volume's data is.
func (RawVolumes) RawMountpoint(ctx context.Context, host, volume string) (string, error) {
	return mountpoint(ctx, host, volume)
}

// MountSources is every host path a running container has bound, on the daemon
// at host.
//
// Asked about unions rather than volumes: a share's union is bound into a
// container by PATH, so no volume filter can find it, and releasing one that a
// container still holds breaks that container for good -- a mount that has gone
// wrong stays wrong until the last container lets go of it.
func (RawVolumes) MountSources(ctx context.Context, host string) (map[string]bool, error) {
	cli := CLI{Host: host}

	ids, err := cli.Line(ctx, "ps", "-q")
	if err != nil {
		return nil, fmt.Errorf("dockercli: listing containers: %w", err)
	}
	if ids == "" {
		return map[string]bool{}, nil
	}

	// A separator inside the template rather than a newline, because whether
	// docker turns a literal \n in --format into one is a claim about the CLI
	// that nothing here checks. A mount source is an absolute path, so a
	// vertical bar cannot appear inside one.
	args := append([]string{"inspect", "--format", "{{range .Mounts}}{{.Source}}|{{end}}"},
		strings.Fields(ids)...)
	out, err := cli.Line(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("dockercli: inspecting containers: %w", err)
	}

	sources := map[string]bool{}
	for _, field := range strings.FieldsFunc(out, func(r rune) bool {
		return r == '|' || r == '\n'
	}) {
		if field = strings.TrimSpace(field); field != "" {
			sources[field] = true
		}
	}
	return sources, nil
}
