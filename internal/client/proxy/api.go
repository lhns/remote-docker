package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/lhns/remote-docker/internal/client/rewrite"
)

// APIClient makes Docker API calls of our own -- creating the volumes that
// back rewritten binds, and reading container state for port forwarding.
//
// It is deliberately tiny rather than the official SDK: we need three calls,
// and the SDK would pull in a dependency tree to make them. Everything the
// user's own tooling does still goes through the proxy untouched.
type APIClient struct {
	Dialer Dialer

	// Version pins the API version in request paths. Empty means unversioned,
	// which the daemon answers with its own default.
	Version string
}

// do performs one request over a fresh connection to the daemon.
func (c *APIClient) do(ctx context.Context, method, path string, body any) (*http.Response, io.Closer, error) {
	conn, err := c.Dialer.DialDocker(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("proxy: connecting to the workspace daemon: %w", err)
	}

	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			conn.Close()
			return nil, nil, fmt.Errorf("proxy: encoding request: %w", err)
		}
		payload = strings.NewReader(string(encoded))
	}

	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+c.versioned(path), payload)
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("proxy: building request: %w", err)
	}
	req.Host = "docker"
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("proxy: sending request: %w", err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("proxy: reading response: %w", err)
	}
	return resp, conn, nil
}

func (c *APIClient) versioned(path string) string {
	if c.Version == "" {
		return path
	}
	return "/" + strings.TrimPrefix(c.Version, "/") + path
}

// EnsureVolume creates an NFS-backed volume if it is not already present.
//
// Docker's volume create is idempotent for an identical definition and returns
// the existing volume, so a reconnecting client re-uses what it made last time
// rather than failing or duplicating.
func (c *APIClient) EnsureVolume(ctx context.Context, name string, driverOpts, labels map[string]string) error {
	body := map[string]any{
		"Name":       name,
		"Driver":     "local",
		"DriverOpts": driverOpts,
		// The labels mark the volume as ours, so garbage collection can find
		// it and -- more importantly -- can tell it apart from a volume the
		// user created, which is never ours to remove.
		"Labels": labels,
	}

	resp, conn, err := c.do(ctx, http.MethodPost, "/volumes/create", body)
	if err != nil {
		return err
	}
	defer conn.Close()
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("proxy: creating volume %s: %s", name, apiError(resp))
	}
	return nil
}

// Container is the subset of container state the port forwarder needs.
type Container struct {
	ID    string `json:"Id"`
	Names []string
	Ports []struct {
		IP          string
		PrivatePort int
		PublicPort  int
		Type        string
	}
	Labels map[string]string
}

// ListContainers returns the running containers.
//
// Used to reconcile port forwards after a dropped event stream: without it,
// forwards leak and containers started during the gap are never forwarded.
func (c *APIClient) ListContainers(ctx context.Context) ([]Container, error) {
	resp, conn, err := c.do(ctx, http.MethodGet, "/containers/json", nil)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("proxy: listing containers: %s", apiError(resp))
	}

	var containers []Container
	if err := json.NewDecoder(resp.Body).Decode(&containers); err != nil {
		return nil, fmt.Errorf("proxy: decoding container list: %w", err)
	}
	return containers, nil
}

// Event is the subset of a Docker event the port forwarder needs.
type Event struct {
	Type   string `json:"Type"`
	Action string `json:"Action"`
	Actor  struct {
		ID         string            `json:"ID"`
		Attributes map[string]string `json:"Attributes"`
	} `json:"Actor"`
}

// Events streams container lifecycle events until ctx is cancelled or the
// stream breaks. The returned closer must be closed by the caller.
func (c *APIClient) Events(ctx context.Context) (<-chan Event, io.Closer, error) {
	filters := url.QueryEscape(`{"type":{"container":true}}`)
	resp, conn, err := c.do(ctx, http.MethodGet, "/events?filters="+filters, nil)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode >= 300 {
		resp.Body.Close()
		conn.Close()
		return nil, nil, fmt.Errorf("proxy: subscribing to events: %s", apiError(resp))
	}

	events := make(chan Event)
	go func() {
		defer close(events)
		defer resp.Body.Close()

		// The daemon streams one JSON object per event with no wrapping array,
		// so a streaming decoder is the right shape here.
		decoder := json.NewDecoder(resp.Body)
		for {
			var event Event
			if err := decoder.Decode(&event); err != nil {
				return
			}
			select {
			case events <- event:
			case <-ctx.Done():
				return
			}
		}
	}()

	return events, conn, nil
}

// apiError extracts the daemon's own message, which is far more useful than
// the status code alone.
func apiError(resp *http.Response) string {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))

	var msg struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &msg); err == nil && msg.Message != "" {
		return msg.Message
	}
	if trimmed := strings.TrimSpace(string(body)); trimmed != "" {
		return fmt.Sprintf("%s: %s", resp.Status, trimmed)
	}
	return resp.Status
}

// Volume is a volume as the daemon reports it.
type Volume struct {
	Name   string
	Labels map[string]string
}

// ListVolumes returns every volume on the workspace daemon.
func (c *APIClient) ListVolumes(ctx context.Context) ([]rewrite.Volume, error) {
	resp, conn, err := c.do(ctx, http.MethodGet, "/volumes", nil)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("proxy: listing volumes: %s", apiError(resp))
	}

	var payload struct {
		Volumes []Volume
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("proxy: decoding volume list: %w", err)
	}

	out := make([]rewrite.Volume, 0, len(payload.Volumes))
	for _, v := range payload.Volumes {
		out = append(out, rewrite.Volume{Name: v.Name, Labels: v.Labels})
	}
	return out, nil
}

// RemoveVolume deletes a volume by name.
func (c *APIClient) RemoveVolume(ctx context.Context, name string) error {
	resp, conn, err := c.do(ctx, http.MethodDelete, "/volumes/"+url.PathEscape(name), nil)
	if err != nil {
		return err
	}
	defer conn.Close()
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("proxy: removing volume %s: %s", name, apiError(resp))
	}
	return nil
}

// VolumesInUse names the volumes referenced by a container, running or not.
//
// Stopped containers count. A volume removed from under a stopped container
// would make it fail to start again with a mount error, which is a confusing
// way to discover that garbage collection was too eager.
func (c *APIClient) VolumesInUse(ctx context.Context) (map[string]bool, error) {
	resp, conn, err := c.do(ctx, http.MethodGet, "/containers/json?all=true", nil)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("proxy: listing containers: %s", apiError(resp))
	}

	var containers []struct {
		Mounts []struct {
			Type string
			Name string
		}
	}
	if err := json.NewDecoder(resp.Body).Decode(&containers); err != nil {
		return nil, fmt.Errorf("proxy: decoding container mounts: %w", err)
	}

	inUse := map[string]bool{}
	for _, container := range containers {
		for _, m := range container.Mounts {
			if m.Type == "volume" && m.Name != "" {
				inUse[m.Name] = true
			}
		}
	}
	return inUse, nil
}
