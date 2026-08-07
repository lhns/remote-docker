package rewrite

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/lhns/remote-docker/pkg/workspace"
)

// Sharer registers a local directory for export and reports where it lands.
//
// An interface rather than the concrete registry so the rewriter can be tested
// without an NFS server, and so registration failures are the rewriter's
// problem rather than the server's.
type Sharer interface {
	// Share exports localPath and returns its export path, e.g. "/m/<id>".
	Share(localPath string) (exportPath string, err error)
}

// VolumeEnsurer creates a volume on the workspace daemon if it is not already
// there.
type VolumeEnsurer interface {
	EnsureVolume(ctx context.Context, name string, driverOpts map[string]string) error
}

// Rewriter converts bind mounts naming local paths into NFS-backed volumes.
type Rewriter struct {
	Shares  Sharer
	Volumes VolumeEnsurer

	// NFSPort is the loopback port inside the workspace where the reverse
	// tunnel exposes this client's NFS server.
	NFSPort int
}

// ContainerCreate rewrites the body of POST /containers/create.
//
// The body is handled as generic JSON, never as a typed struct. Decoding into
// Go types and re-encoding would silently drop every field those types do not
// know about, so a client newer than us would lose configuration it set --
// health checks, resource limits, whatever the API gained last release. Only
// the two fields that carry bind mounts are touched; everything else is
// re-encoded exactly as it arrived.
func (r *Rewriter) ContainerCreate(ctx context.Context, body []byte) ([]byte, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("rewrite: decoding container create: %w", err)
	}

	hostConfigRaw, ok := payload["HostConfig"]
	if !ok {
		// No HostConfig means no binds. Nothing to do, and the body must go
		// through byte-identical.
		return body, nil
	}

	var hostConfig map[string]json.RawMessage
	if err := json.Unmarshal(hostConfigRaw, &hostConfig); err != nil {
		return nil, fmt.Errorf("rewrite: decoding HostConfig: %w", err)
	}

	changed := false

	if err := r.rewriteBinds(ctx, hostConfig, &changed); err != nil {
		return nil, err
	}
	if err := r.rewriteMounts(ctx, hostConfig, &changed); err != nil {
		return nil, err
	}
	if !changed {
		return body, nil
	}

	newHostConfig, err := json.Marshal(hostConfig)
	if err != nil {
		return nil, fmt.Errorf("rewrite: encoding HostConfig: %w", err)
	}
	payload["HostConfig"] = newHostConfig

	out, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("rewrite: encoding container create: %w", err)
	}
	return out, nil
}

// rewriteBinds handles HostConfig.Binds, the `-v` form.
func (r *Rewriter) rewriteBinds(ctx context.Context, hostConfig map[string]json.RawMessage, changed *bool) error {
	raw, ok := hostConfig["Binds"]
	if !ok || string(raw) == "null" {
		return nil
	}

	var binds []string
	if err := json.Unmarshal(raw, &binds); err != nil {
		return fmt.Errorf("rewrite: decoding Binds: %w", err)
	}

	for i, spec := range binds {
		parsed, err := ParseBind(spec)
		if err != nil {
			// Not something we understand. Forward it and let the daemon
			// produce its own error, which will be about the actual problem.
			continue
		}
		if !IsLocalPath(parsed.Source) {
			// A named volume. Leave it entirely alone -- rewriting one would
			// replace the user's persistent data with an export of a
			// directory that does not exist.
			continue
		}

		volume, err := r.volumeFor(ctx, parsed.Source)
		if err != nil {
			return err
		}
		parsed.Source = volume
		binds[i] = parsed.String()
		*changed = true
	}

	if !*changed {
		return nil
	}
	encoded, err := json.Marshal(binds)
	if err != nil {
		return fmt.Errorf("rewrite: encoding Binds: %w", err)
	}
	hostConfig["Binds"] = encoded
	return nil
}

// rewriteMounts handles HostConfig.Mounts, the `--mount` form that Compose and
// the API-level clients prefer.
func (r *Rewriter) rewriteMounts(ctx context.Context, hostConfig map[string]json.RawMessage, changed *bool) error {
	raw, ok := hostConfig["Mounts"]
	if !ok || string(raw) == "null" {
		return nil
	}

	// Each mount stays generic for the same reason the envelope does: a mount
	// carries BindOptions, VolumeOptions, TmpfsOptions and Consistency, and
	// dropping any of them changes the mount.
	var mounts []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &mounts); err != nil {
		return fmt.Errorf("rewrite: decoding Mounts: %w", err)
	}

	touched := false
	for _, mount := range mounts {
		var mountType string
		if err := json.Unmarshal(mount["Type"], &mountType); err != nil {
			continue
		}
		if mountType != "bind" {
			// volume, tmpfs, npipe, cluster -- none of them name a path on
			// this machine.
			continue
		}

		var source string
		if err := json.Unmarshal(mount["Source"], &source); err != nil {
			continue
		}
		if !IsLocalPath(source) {
			continue
		}

		volume, err := r.volumeFor(ctx, source)
		if err != nil {
			return err
		}

		mount["Type"] = json.RawMessage(`"volume"`)
		encodedSource, err := json.Marshal(volume)
		if err != nil {
			return fmt.Errorf("rewrite: encoding mount source: %w", err)
		}
		mount["Source"] = encodedSource

		// BindOptions describes propagation for a bind, and the daemon
		// rejects it on a volume mount. The propagation it asks for is
		// meaningless here anyway: the volume is mounted inside the daemon's
		// own namespace when the container starts.
		delete(mount, "BindOptions")

		touched = true
	}

	if !touched {
		return nil
	}
	encoded, err := json.Marshal(mounts)
	if err != nil {
		return fmt.Errorf("rewrite: encoding Mounts: %w", err)
	}
	hostConfig["Mounts"] = encoded
	*changed = true
	return nil
}

// volumeFor exports a local directory and returns the name of the volume
// backing it on the workspace, creating that volume if needed.
func (r *Rewriter) volumeFor(ctx context.Context, localPath string) (string, error) {
	exportPath, err := r.Shares.Share(localPath)
	if err != nil {
		return "", fmt.Errorf("rewrite: exporting %s: %w", localPath, err)
	}

	name, err := workspace.VolumeNameForExport(exportPath)
	if err != nil {
		return "", fmt.Errorf("rewrite: %w", err)
	}

	opts := workspace.NFSVolumeOptions(r.NFSPort, exportPath)
	if err := r.Volumes.EnsureVolume(ctx, name, opts); err != nil {
		return "", fmt.Errorf("rewrite: creating volume for %s: %w", localPath, err)
	}
	return name, nil
}
