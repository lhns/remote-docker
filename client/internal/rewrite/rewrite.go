package rewrite

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/lhns/remote-docker/pkg/workspace"
)

// Guard keeps garbage collection off the volume a bind rewrite is in the
// middle of creating.
//
// The two halves of a volume's life do not overlap in the DAEMON's view: it
// learns a volume is in use only when a container referencing it is created,
// which is strictly after the volume exists. Between those two moments the
// volume is ours, needed, and reported as unused, and the collector runs
// exactly then, because the connection it rides on is opened lazily by the
// very request that is creating the volume.
//
// What happens when it loses is silent and looks like the file server broke:
// the daemon RECREATES a missing named volume as an empty local one, so the
// container starts with an empty directory where the user's project should be
// and the first thing to read a file reports it missing. `remote-docker start
// && docker run -v $PWD:/w` failed that way in CI.
//
// Exported answers whether a volume backs a directory this session is
// exporting, which is the fact the daemon cannot know. The lock closes the
// remaining window: a removal decides under it, and a rewrite holds it across
// registering the share and creating the volume. Whichever goes first, the
// other sees a settled world: either the share is registered and the volume is
// spared, or the volume goes and is immediately recreated.
type Guard struct {
	mu sync.Mutex

	// Exported reports whether a volume name backs a currently exported
	// directory. Nil means nothing is exported, which is what a rewriter
	// without a session looks like.
	Exported func(volume string) bool
}

// hold locks the guard and returns its release. A nil guard is a working
// no-op, so a Rewriter or Collector built without one, which is every unit
// test that does not care, behaves as it did before.
func (g *Guard) hold() func() {
	if g == nil {
		return func() {}
	}
	g.mu.Lock()
	return g.mu.Unlock
}

// exported reports whether the volume backs a directory this session exports.
func (g *Guard) exported(volume string) bool {
	if g == nil || g.Exported == nil {
		return false
	}
	return g.Exported(volume)
}

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
	EnsureVolume(ctx context.Context, name string, driverOpts, labels map[string]string) error
}

// ManagedLabel marks a volume as one this client created, and is what makes
// garbage collection safe: the rd- prefix alone proves nothing, since a user
// is entitled to name a volume "rd-backups".
const ManagedLabel = "com.github.lhns.remote-docker"

// Rewriter converts bind mounts naming local paths into NFS-backed volumes.
type Rewriter struct {
	Shares  Sharer
	Volumes VolumeEnsurer

	// NFSPort is the loopback port inside the workspace where the reverse
	// tunnel exposes this client's NFS server.
	NFSPort int

	// Owner identifies this client's containers on a daemon shared with other
	// accounts. Empty disables labelling.
	Owner string

	// Guard is shared with the Collector, and is what stops one deleting the
	// volume the other has just created.
	Guard *Guard
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

	changed := false
	if err := r.label(payload, &changed); err != nil {
		return nil, err
	}

	hostConfigRaw, ok := payload["HostConfig"]
	if !ok {
		// No HostConfig means no binds, but the label above may still have
		// changed the payload.
		if !changed {
			return body, nil
		}
		out, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("rewrite: encoding container create: %w", err)
		}
		return out, nil
	}

	var hostConfig map[string]json.RawMessage
	if err := json.Unmarshal(hostConfigRaw, &hostConfig); err != nil {
		return nil, fmt.Errorf("rewrite: decoding HostConfig: %w", err)
	}

	hostChanged := false
	if err := r.rewriteBinds(ctx, hostConfig, &hostChanged); err != nil {
		return nil, err
	}
	if err := r.rewriteMounts(ctx, hostConfig, &hostChanged); err != nil {
		return nil, err
	}
	if !hostChanged && !changed {
		return body, nil
	}

	if hostChanged {
		newHostConfig, err := json.Marshal(hostConfig)
		if err != nil {
			return nil, fmt.Errorf("rewrite: encoding HostConfig: %w", err)
		}
		payload["HostConfig"] = newHostConfig
	}

	out, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("rewrite: encoding container create: %w", err)
	}
	return out, nil
}

// OwnerLabel marks every container this client creates.
//
// The workspace daemon is shared between accounts (ADR 0012), so its event
// stream carries other people's containers. Without a mark of our own, port
// forwarding would open listeners on this machine because somebody else ran
// docker compose up.
const OwnerLabel = "com.github.lhns.remote-docker.owner"

// label stamps OwnerLabel onto the container's labels, preserving any the
// caller set.
func (r *Rewriter) label(payload map[string]json.RawMessage, changed *bool) error {
	if r.Owner == "" {
		return nil
	}

	labels := map[string]string{}
	if raw, ok := payload["Labels"]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &labels); err != nil {
			// Labels we cannot read are left alone rather than replaced; the
			// daemon will report anything genuinely malformed.
			return nil
		}
	}
	if labels[OwnerLabel] == r.Owner {
		return nil
	}
	labels[OwnerLabel] = r.Owner

	encoded, err := json.Marshal(labels)
	if err != nil {
		return fmt.Errorf("rewrite: encoding labels: %w", err)
	}
	payload["Labels"] = encoded
	*changed = true
	return nil
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
			// A named volume. Left alone: rewriting one would replace the
			// user's persistent data with an export of a directory that does
			// not exist.
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
			// volume, tmpfs, npipe and cluster name no path on this machine.
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
	// Held across BOTH steps: registering the share is what tells the collector
	// this volume is spoken for, and the volume does not exist until the step
	// after it.
	defer r.Guard.hold()()

	exportPath, err := r.Shares.Share(localPath)
	if err != nil {
		return "", fmt.Errorf("rewrite: exporting %s: %w", localPath, err)
	}

	name, err := workspace.VolumeNameForExport(exportPath)
	if err != nil {
		return "", fmt.Errorf("rewrite: %w", err)
	}

	opts := workspace.NFSVolumeOptions(r.NFSPort, exportPath)
	labels := map[string]string{ManagedLabel: "share"}
	if r.Owner != "" {
		labels[OwnerLabel] = r.Owner
	}
	if err := r.Volumes.EnsureVolume(ctx, name, opts, labels); err != nil {
		return "", fmt.Errorf("rewrite: creating volume for %s: %w", localPath, err)
	}
	return name, nil
}
