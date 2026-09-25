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

	"github.com/lhns/remote-docker/client/internal/rewrite"
	"github.com/lhns/remote-docker/core/workspace"

	"maps"
)

// APIClient makes Docker API calls of our own. Deliberately tiny rather than
// the SDK, which would pull in a dependency tree for a handful of calls.
type APIClient struct {
	Dialer Dialer
}

// do performs one request over a fresh connection to the daemon. No Transport
// honours ctx here, so closeOnCancel is the only thing stopping a silent
// workspace from blocking the caller (and idle release) forever.
func (c *APIClient) do(ctx context.Context, method, path string, body any) (*http.Response, io.Closer, error) {
	conn, err := c.Dialer.DialDocker(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("proxy: connecting to the workspace daemon: %w", err)
	}
	stop := closeOnCancel(ctx, conn)

	fail := func(format string, err error) (*http.Response, io.Closer, error) {
		stop()
		conn.Close()
		return nil, nil, fmt.Errorf(format, err)
	}

	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fail("proxy: encoding request: %w", err)
		}
		payload = strings.NewReader(string(encoded))
	}

	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, payload)
	if err != nil {
		return fail("proxy: building request: %w", err)
	}
	req.Host = "docker"
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	if err := req.Write(conn); err != nil {
		return fail("proxy: sending request: %w", err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		return fail("proxy: reading response: %w", err)
	}

	// Not deferred: the connection outlives this call (Events keeps reading).
	stop()
	return resp, conn, nil
}

// call makes one request and decodes a successful reply into out, unless out
// is nil. Errors read "proxy: <what>: ..." and "proxy: decoding <decoded>: ...".
func (c *APIClient) call(ctx context.Context, method, path string, body, out any, what, decoded string) error {
	resp, conn, err := c.do(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer conn.Close()
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("proxy: %s: %s", what, apiError(resp))
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("proxy: decoding %s: %w", decoded, err)
	}
	return nil
}

// closeOnCancel closes conn when ctx is done, until stop is called. stop waits
// for the goroutine, or one would leak per call under the session's context.
func closeOnCancel(ctx context.Context, conn io.Closer) (stop func()) {
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}

// EnsureVolume creates an NFS-backed volume, replacing one whose options have
// drifted: volume create on an existing name silently ignores the options.
func (c *APIClient) EnsureVolume(ctx context.Context, name string, driverOpts, labels map[string]string) error {
	if err := c.replaceIfStale(ctx, name, driverOpts); err != nil {
		return err
	}

	body := map[string]any{
		"Name":       name,
		"Driver":     "local",
		"DriverOpts": driverOpts,
		"Labels":     labels,
	}

	return c.call(ctx, http.MethodPost, "/volumes/create", body, nil, "creating volume "+name, "")
}

// replaceIfStale removes a managed volume whose driver options have changed,
// only if it is ours and no container holds it.
func (c *APIClient) replaceIfStale(ctx context.Context, name string, want map[string]string) error {
	existing, ok := c.inspectVolume(ctx, name)
	if !ok {
		return nil
	}
	if existing.Labels[workspace.ManagedLabel] != workspace.ManagedShare {
		return nil
	}
	if maps.Equal(existing.Options, want) {
		return nil
	}

	inUse, err := c.VolumesInUse(ctx)
	if err == nil && inUse[name] {
		return fmt.Errorf("the volume %s was built for a different export and a container is using it\n"+
			"  fix: stop and remove that container, and it will be rebuilt", name)
	}
	return c.RemoveVolume(ctx, name)
}

type volumeDetail struct {
	Options map[string]string `json:"Options"`
	Labels  map[string]string `json:"Labels"`
}

// inspectVolume returns a volume's definition. Any failure is just false:
// volume create will report what is wrong.
func (c *APIClient) inspectVolume(ctx context.Context, name string) (volumeDetail, bool) {
	var detail volumeDetail
	err := c.call(ctx, http.MethodGet, "/volumes/"+url.PathEscape(name), nil, &detail, "inspecting volume "+name, "volume "+name)
	return detail, err == nil
}

// Container is the subset of container state the client needs.
type Container struct {
	ID    string `json:"Id"`
	Names []string
	Ports []struct {
		PrivatePort int
		PublicPort  int
		Type        string
	}
	Labels map[string]string

	// Mounts decides idle release: a running container on one of our volumes
	// holds a live NFS mount.
	Mounts []struct {
		Type string
		Name string
	}
}

// ListContainers returns the running containers.
func (c *APIClient) ListContainers(ctx context.Context) ([]Container, error) {
	var containers []Container
	if err := c.call(ctx, http.MethodGet, "/containers/json", nil, &containers, "listing containers", "container list"); err != nil {
		return nil, err
	}
	return containers, nil
}

// Event is the subset of a Docker event the port forwarder needs.
type Event struct {
	Action string `json:"Action"`
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

// apiError extracts the daemon's own message.
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

// ListVolumes returns every volume on the workspace daemon.
func (c *APIClient) ListVolumes(ctx context.Context) ([]rewrite.Volume, error) {
	var payload struct {
		Volumes []rewrite.Volume
	}
	if err := c.call(ctx, http.MethodGet, "/volumes", nil, &payload, "listing volumes", "volume list"); err != nil {
		return nil, err
	}
	return payload.Volumes, nil
}

// RemoveVolume deletes a volume by name.
func (c *APIClient) RemoveVolume(ctx context.Context, name string) error {
	return c.call(ctx, http.MethodDelete, "/volumes/"+url.PathEscape(name), nil, nil, "removing volume "+name, "")
}

// VolumesInUse names the volumes referenced by any container, stopped ones
// included: removing one would fail that container's next start.
func (c *APIClient) VolumesInUse(ctx context.Context) (map[string]bool, error) {
	var containers []Container
	if err := c.call(ctx, http.MethodGet, "/containers/json?all=true", nil, &containers, "listing containers", "container mounts"); err != nil {
		return nil, err
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
