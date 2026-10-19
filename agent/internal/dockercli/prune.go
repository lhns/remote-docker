package dockercli

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/lhns/remote-docker/agent/internal/prune"
)

// Pruning is what the agent asks a daemon for in order to prune it (ADR 0056).
// Every method takes the daemon as a -H value, empty for the default socket.
// Nothing here removes a volume or forces anything.
type Pruning struct{}

// imageFormat prints an image as one tab-separated line. The tags and digests
// are JSON arrays, since a name may hold anything but a tab or a newline.
const imageFormat = "{{.Id}}\t{{json .RepoTags}}\t{{json .RepoDigests}}\t{{.Size}}"

// Images is every image, intermediate ones included.
func (Pruning) Images(ctx context.Context, host string) ([]prune.Image, error) {
	cli := CLI{Host: host}
	list, err := cli.Line(ctx, "image", "ls", "--all", "--quiet", "--no-trunc")
	if err != nil {
		return nil, fmt.Errorf("dockercli: listing images: %w", err)
	}
	refs := uniqueFields(list)
	if len(refs) == 0 {
		return nil, nil
	}
	out, err := cli.Line(ctx, append([]string{"image", "inspect", "--format", imageFormat}, refs...)...)
	if err != nil {
		return nil, fmt.Errorf("dockercli: inspecting images: %w", err)
	}
	return parseImages(out)
}

// uniqueFields splits on whitespace, keeping the first of each word.
func uniqueFields(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, f := range strings.Fields(s) {
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

func parseImages(out string) ([]prune.Image, error) {
	var images []prune.Image
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 4 {
			return nil, fmt.Errorf("dockercli: reading an image: %q", line)
		}
		img := prune.Image{ID: f[0]}
		if err := json.Unmarshal([]byte(f[1]), &img.Tags); err != nil {
			return nil, fmt.Errorf("dockercli: reading the tags of %s: %w", f[0], err)
		}
		if err := json.Unmarshal([]byte(f[2]), &img.Digests); err != nil {
			return nil, fmt.Errorf("dockercli: reading the digests of %s: %w", f[0], err)
		}
		img.Size, _ = strconv.ParseInt(f[3], 10, 64)
		images = append(images, img)
	}
	return images, nil
}

// containerFormat prints a container as one tab-separated line.
const containerFormat = "{{.ID}}\t{{.Name}}\t{{.Image}}\t{{.State.Status}}\t{{.Created}}\t{{.State.FinishedAt}}"

// Containers is every container, running or not.
func (Pruning) Containers(ctx context.Context, host string) ([]prune.Container, error) {
	cli := CLI{Host: host}
	list, err := cli.Line(ctx, "ps", "--all", "--quiet", "--no-trunc")
	if err != nil {
		return nil, fmt.Errorf("dockercli: listing containers: %w", err)
	}
	ids := uniqueFields(list)
	if len(ids) == 0 {
		return nil, nil
	}
	out, err := cli.Line(ctx, append([]string{"inspect", "--type", "container", "--format", containerFormat}, ids...)...)
	if err != nil {
		return nil, fmt.Errorf("dockercli: inspecting containers: %w", err)
	}
	return parseContainers(out)
}

func parseContainers(out string) ([]prune.Container, error) {
	var cs []prune.Container
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 6 {
			return nil, fmt.Errorf("dockercli: reading a container: %q", line)
		}
		c := prune.Container{ID: f[0], Name: f[1], ImageID: f[2], Status: f[3]}
		c.Created, _ = time.Parse(time.RFC3339Nano, f[4])
		c.Finished, _ = time.Parse(time.RFC3339Nano, f[5])
		cs = append(cs, c)
	}
	return cs, nil
}

// CreatedImages is the image of every container created in [since, until], as
// the user typed it. The daemon keeps only its last few hundred events, so this
// is best effort.
func (Pruning) CreatedImages(ctx context.Context, host string, since, until time.Time) ([]string, error) {
	out, err := CLI{Host: host}.Line(ctx, "events",
		"--since", strconv.FormatInt(since.Unix(), 10),
		"--until", strconv.FormatInt(until.Unix(), 10),
		"--filter", "type=container", "--filter", "event=create",
		"--format", "{{.Actor.Attributes.image}}")
	if err != nil {
		return nil, fmt.Errorf("dockercli: reading events: %w", err)
	}
	return uniqueFields(out), nil
}

// RemoveImage removes one reference to an image. Never forced: the daemon
// refuses an image a container still uses, stopped or not.
func (Pruning) RemoveImage(ctx context.Context, host, ref string) error {
	return CLI{Host: host}.Run(ctx, "dockercli: removing image "+ref, "image", "rm", ref)
}

// RemoveContainer removes a stopped container, keeping its volumes.
func (Pruning) RemoveContainer(ctx context.Context, host, id string) error {
	return CLI{Host: host}.Run(ctx, "dockercli: removing container "+id, "rm", id)
}

// BuildPrune removes build cache not used for unusedFor.
func (Pruning) BuildPrune(ctx context.Context, host string, unusedFor time.Duration) error {
	return CLI{Host: host}.Run(ctx, "dockercli: pruning the build cache",
		"builder", "prune", "--force", "--filter", "until="+unusedFor.Round(time.Second).String())
}

// RootDir is where the daemon keeps its data, as the agent sees that path.
func (Pruning) RootDir(ctx context.Context, host string) (string, error) {
	dir, err := CLI{Host: host}.Line(ctx, "info", "--format", "{{.DockerRootDir}}")
	if err != nil {
		return "", fmt.Errorf("dockercli: asking for the root directory: %w", err)
	}
	if dir == "" {
		return "", fmt.Errorf("dockercli: the daemon reported no root directory")
	}
	return dir, nil
}
