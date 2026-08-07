package notify

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// DockerVolumes resolves a volume to its mountpoint through the docker CLI,
// which the workspace image already carries. The agent has no Go Docker
// client, and adding one to ask a single question would be a large dependency
// for a `--format` string -- the same trade internal/server/elevate makes.
type DockerVolumes struct {
	// Docker is the binary to run. Empty means "docker" from PATH.
	Docker string
}

func (d DockerVolumes) Mountpoint(ctx context.Context, volume string) (string, error) {
	bin := d.Docker
	if bin == "" {
		bin = "docker"
	}
	out, err := exec.CommandContext(ctx, bin,
		"volume", "inspect", volume, "--format", "{{.Mountpoint}}").Output()
	if err != nil {
		return "", fmt.Errorf("notify: inspecting volume %s: %w", volume, err)
	}
	mp := strings.TrimSpace(string(out))
	if mp == "" {
		return "", fmt.Errorf("notify: volume %s reported no mountpoint", volume)
	}
	return mp, nil
}
