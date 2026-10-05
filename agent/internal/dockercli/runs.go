package dockercli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/lhns/remote-docker/core/workspace"
)

// composeProject is the label compose puts on what it creates.
const composeProject = "com.docker.compose.project"

// Volume is a volume as the daemon reports it.
type Volume struct {
	Name   string
	Labels map[string]string
}

// RunObjects finds and removes what an ephemeral run left on a daemon (ADR
// 0050). Every method takes the daemon as a -H value, empty for the shared one.
type RunObjects struct{}

// Containers is every container, running or not, labelled with this account
// and client.
func (RunObjects) Containers(ctx context.Context, host, account, client string) ([]string, error) {
	out, err := CLI{Host: host}.Line(ctx, "ps", "--all", "--quiet", "--no-trunc",
		"--filter", "label="+workspace.OwnerLabel+"="+account,
		"--filter", "label="+workspace.ClientLabel+"="+client)
	if err != nil {
		return nil, fmt.Errorf("dockercli: listing containers: %w", err)
	}
	return strings.Fields(out), nil
}

// RemoveContainers removes containers, stopping them first, with their
// anonymous volumes.
func (RunObjects) RemoveContainers(ctx context.Context, host string, ids []string) error {
	return CLI{Host: host}.Run(ctx, "dockercli: removing containers",
		append([]string{"rm", "--force", "--volumes"}, ids...)...)
}

// Networks is every network compose made for a project whose name ends in
// -<client>.
func (RunObjects) Networks(ctx context.Context, host, client string) ([]string, error) {
	out, err := CLI{Host: host}.Line(ctx, "network", "ls",
		"--filter", "label="+composeProject,
		"--format", `{{.Name}}|{{.Label "`+composeProject+`"}}`)
	if err != nil {
		return nil, fmt.Errorf("dockercli: listing networks: %w", err)
	}
	return projectNetworks(out, client), nil
}

// projectNetworks picks, from name|project lines, the networks of a project
// named for this client.
func projectNetworks(out, client string) []string {
	var names []string
	for _, line := range strings.Split(out, "\n") {
		name, project, ok := strings.Cut(strings.TrimSpace(line), "|")
		if ok && strings.HasSuffix(project, "-"+client) {
			names = append(names, name)
		}
	}
	return names
}

// RemoveNetwork removes a network, which the daemon refuses while anything is
// attached to it.
func (RunObjects) RemoveNetwork(ctx context.Context, host, name string) error {
	return CLI{Host: host}.Run(ctx, "dockercli: removing network "+name, "network", "rm", name)
}

// Volumes is every volume labelled with this client, with its labels.
func (RunObjects) Volumes(ctx context.Context, host, client string) ([]Volume, error) {
	cli := CLI{Host: host}
	names, err := cli.Line(ctx, "volume", "ls", "--quiet",
		"--filter", "label="+workspace.ClientLabel+"="+client)
	if err != nil {
		return nil, fmt.Errorf("dockercli: listing volumes: %w", err)
	}
	if names == "" {
		return nil, nil
	}
	args := append([]string{"volume", "inspect", "--format", `{{json .}}`}, strings.Fields(names)...)
	out, err := cli.Line(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("dockercli: inspecting volumes: %w", err)
	}
	var vols []Volume
	for _, line := range strings.Split(out, "\n") {
		var v Volume
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			return nil, fmt.Errorf("dockercli: reading a volume: %w", err)
		}
		vols = append(vols, v)
	}
	return vols, nil
}

// VolumesInUse is every volume a container names, running or not.
func (RunObjects) VolumesInUse(ctx context.Context, host string) (map[string]bool, error) {
	cli := CLI{Host: host}
	ids, err := cli.Line(ctx, "ps", "--all", "--quiet")
	if err != nil {
		return nil, fmt.Errorf("dockercli: listing containers: %w", err)
	}
	inUse := map[string]bool{}
	if ids == "" {
		return inUse, nil
	}
	args := append([]string{"inspect", "--format",
		`{{range .Mounts}}{{if eq .Type "volume"}}{{.Name}}|{{end}}{{end}}`},
		strings.Fields(ids)...)
	out, err := cli.Line(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("dockercli: inspecting containers: %w", err)
	}
	for _, name := range strings.FieldsFunc(out, func(r rune) bool { return r == '|' || r == '\n' }) {
		if name = strings.TrimSpace(name); name != "" {
			inUse[name] = true
		}
	}
	return inUse, nil
}

// RemoveVolume removes a volume, which the daemon refuses while a container
// names it.
func (RunObjects) RemoveVolume(ctx context.Context, host, name string) error {
	return CLI{Host: host}.Run(ctx, "dockercli: removing volume "+name, "volume", "rm", name)
}
