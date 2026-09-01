package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/lhns/remote-docker/core/workspace"
)

// Filling a volume with the client's files, for the delegated consistency
// (ADR 0043).
//
// There is no API that writes into a volume. `PUT /containers/{id}/archive` is
// the only way in -- it is what `docker cp` uses -- so a container has to exist
// with the volume mounted, and it does not have to run: the daemon mounts a
// created container's volumes to resolve the path.
//
// The image is the one the caller's own container is about to use, which is
// the one image this program can be sure the daemon has. Anything else would
// mean a pull, on a workspace that may have no registry access at all.

// seedTarget is where the volume is mounted inside the temporary container.
// Any absolute path does; this one names itself in a `docker ps` that catches
// the container mid-life.
const seedTarget = "/rd-seed"

// SeedVolume writes a tar stream into a volume.
//
// The tar is read as it is sent, so a large tree is streamed rather than held
// in memory, and the request is chunked because its size is not known until the
// walk finishes.
func (c *APIClient) SeedVolume(ctx context.Context, image, volume string, tree io.Reader) error {
	id, err := c.createSeedContainer(ctx, image, volume)
	if err != nil {
		return err
	}
	// Always, including on the error path: a container left behind holds the
	// volume, and the next thing to want that volume reports it as in use.
	defer func() {
		if err := c.removeContainer(context.WithoutCancel(ctx), id); err != nil {
			// Nothing the caller can do, and the seed itself succeeded or
			// failed on its own terms.
			_ = err
		}
	}()

	return c.putArchive(ctx, id, seedTarget, tree)
}

// createSeedContainer makes the container whose only purpose is to have the
// volume mounted. It is never started.
func (c *APIClient) createSeedContainer(ctx context.Context, image, volume string) (string, error) {
	body := map[string]any{
		"Image": image,
		// Marked as ours so a container orphaned by a crash is recognisable,
		// and so it is never mistaken for something the user started.
		"Labels": map[string]string{
			workspace.ManagedLabel: workspace.ManagedSeed,
		},
		"HostConfig": map[string]any{
			"Binds":      []string{volume + ":" + seedTarget},
			"AutoRemove": false,
		},
	}

	resp, conn, err := c.do(ctx, http.MethodPost, "/containers/create", body)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("proxy: creating a container to fill %s: %s", volume, apiError(resp))
	}
	var created struct{ Id string }
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		return "", fmt.Errorf("proxy: decoding the seed container: %w", err)
	}
	if created.Id == "" {
		return "", fmt.Errorf("proxy: the daemon created a container for %s with no id", volume)
	}
	return created.Id, nil
}

func (c *APIClient) removeContainer(ctx context.Context, id string) error {
	resp, conn, err := c.do(ctx, http.MethodDelete, "/containers/"+url.PathEscape(id)+"?v=false&force=true", nil)
	if err != nil {
		return err
	}
	defer conn.Close()
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("proxy: removing the seed container: %s", apiError(resp))
	}
	return nil
}

// putArchive extracts a tar stream into a path in a container.
//
// Not written through do, which marshals JSON and knows a body's length. This
// one is a stream of unknown size, so it is chunked, exactly as `docker cp`
// sends it.
func (c *APIClient) putArchive(ctx context.Context, id, path string, tree io.Reader) error {
	conn, err := c.Dialer.DialDocker(ctx)
	if err != nil {
		return fmt.Errorf("proxy: connecting to the workspace daemon: %w", err)
	}
	defer conn.Close()

	target := "/containers/" + url.PathEscape(id) + "/archive?path=" + url.QueryEscape(path)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://docker"+target, io.NopCloser(tree))
	if err != nil {
		return fmt.Errorf("proxy: building the archive request: %w", err)
	}
	req.Host = "docker"
	req.Header.Set("Content-Type", "application/x-tar")
	// Unknown until the walk ends, which is what makes this chunked.
	req.ContentLength = -1

	if err := req.Write(conn); err != nil {
		return fmt.Errorf("proxy: sending the archive: %w", err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		return fmt.Errorf("proxy: reading the archive response: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("proxy: filling the volume: %s", apiError(resp))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// ResetVolume empties a managed volume by removing it, so that what fills it
// next is the whole of what it holds.
//
// Three refusals, all of them "leave it alone and say nothing": a volume that
// is not there, one this client did not create, and one a container is holding.
// The last is the interesting one -- a copy in use cannot be emptied, and the
// caller fills over it instead, which is the one case where a file deleted on
// this machine stays in the copy (ADR 0043).
func (c *APIClient) ResetVolume(ctx context.Context, name string) error {
	existing, ok := c.inspectVolume(ctx, name)
	if !ok || existing.Labels[workspace.ManagedLabel] != workspace.ManagedShare {
		return nil
	}

	inUse, err := c.VolumesInUse(ctx)
	if err != nil {
		return fmt.Errorf("proxy: asking what holds %s: %w", name, err)
	}
	if inUse[name] {
		return nil
	}
	return c.RemoveVolume(ctx, name)
}
