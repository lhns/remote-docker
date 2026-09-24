package dockercli

import (
	"context"
	"fmt"
	"strings"

	"github.com/lhns/remote-docker/core-agent/replay"
)

// Volumes answers replay.Volumes: a managed volume's mountpoint, as the agent
// can reach it. The only part of replaying that knows Docker exists.
type Volumes struct {
	// Host is the daemon to ask, as a -H value; empty or nil is the agent's
	// own (ADR 0012). Lazy, so the account's daemon starts when something is
	// first replayed rather than when the notify session opens.
	Host func() (string, error)

	// Root maps that daemon's filesystem into ours: a per-account daemon
	// reports a mountpoint in its own, reached through /proc/<pid>/root. A
	// func because the pid changes on every restart. Nil means ours already.
	Root func() (string, error)
}

func (v Volumes) Mountpoint(ctx context.Context, volume string) (string, error) {
	host, err := call(v.Host)
	if err != nil {
		// Refused rather than falling back to "", which is the AGENT's daemon
		// and holds a different set of volumes.
		return "", fmt.Errorf("dockercli: locating the daemon holding volume %s: %w", volume, err)
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
// filesystem, unrelocated, for a union: it is mounted in the daemon's mount
// namespace (ADR 0044), where a /proc/<pid>/root path names nothing.
type RawVolumes struct{}

// RawMountpoint asks the daemon at host where a volume's data is.
func (RawVolumes) RawMountpoint(ctx context.Context, host, volume string) (string, error) {
	return mountpoint(ctx, host, volume)
}

// MountSources is every host path a running container has bound, on the daemon
// at host: a union is bound by PATH, so no volume filter finds one.
func (RawVolumes) MountSources(ctx context.Context, host string) (map[string]bool, error) {
	cli := CLI{Host: host}

	ids, err := cli.Line(ctx, "ps", "-q")
	if err != nil {
		return nil, fmt.Errorf("dockercli: listing containers: %w", err)
	}
	if ids == "" {
		return map[string]bool{}, nil
	}

	// A separator in the template rather than a \n, whose handling by --format
	// nothing here checks. A path holding a '|' is split, which matters to
	// nobody: the only sources looked up are union mountpoints, which hold none.
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
