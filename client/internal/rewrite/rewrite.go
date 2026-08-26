package rewrite

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"

	"github.com/lhns/remote-docker/core/workspace"
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
	// Share exports localPath and returns its export path, e.g. "/m/<id>",
	// and, when localPath is a single file, the base name it is exported
	// under. A file's export is a synthesised directory holding only that
	// name, and the mount carries the name as a volume subpath (ADR 0039).
	Share(localPath string) (exportPath, file string, err error)
}

// VolumeEnsurer creates a volume on the workspace daemon if it is not already
// there.
type VolumeEnsurer interface {
	EnsureVolume(ctx context.Context, name string, driverOpts, labels map[string]string) error
}

// The labels this package stamps. Defined in the contract, because the agent
// reads them back (workspace.ClientLabel), so both ends must agree.
const (
	ManagedLabel = workspace.ManagedLabel
	ManagedShare = workspace.ManagedShare
	OwnerLabel   = workspace.OwnerLabel
	ClientLabel  = workspace.ClientLabel
	PortsLabel   = workspace.PortsLabel
)

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

	// Client identifies THIS MACHINE, as distinct from the account. Two of
	// somebody's machines share an account and therefore a daemon, but the
	// files behind a share are on one of them, so the volumes are named and
	// labelled per client. Empty produces the unqualified names, which is what
	// a volume created by a client that predates this carries.
	Client string

	// Guard is shared with the Collector, and is what stops one deleting the
	// volume the other has just created.
	Guard *Guard

	// LocalPortFree reports whether this machine can open a port, and is asked
	// before a published port is handed to the daemon to choose. See
	// rewritePorts, which explains why the question moved here.
	//
	// Nil skips it, which is what a rewriter with no session behind it wants.
	LocalPortFree func(port int) error

	// DockerVersion is what the workspace reports its daemon to be. Read for
	// one question: whether a single file can be mounted at all (ADR 0039).
	// Empty means unknown, which is treated as capable.
	DockerVersion string

	// DaemonPaths are paths the workspace's daemon resolves for itself, from
	// workspace-info (ADR 0041). A bind naming one is passed through untouched,
	// which is what lets kind mount /lib/modules.
	DaemonPaths []string

	// LocalExists reports whether a path is on THIS machine. Nil means os.Stat.
	LocalExists func(path string) bool

	// PosixSource reports the POSIX path a shell may have rewritten a bind
	// source into, and "" when it did not. Git Bash converts BOTH halves of a
	// `-v`, and only the container side can be restored blind (ADR 0040), so a
	// workspace path typed there arrives as a Windows path and would otherwise
	// match nothing here. Nil disables the second reading.
	PosixSource func(source string) string
}

// ownedByDaemon reports whether the workspace resolves this source itself.
//
// Asked only of a source this machine does not have, which is what keeps a typo
// failing: it matches nothing, so it is exported and refused as before.
//
// Slashes by hand rather than filepath, which follows the HOST's rules: a
// Windows source compared on a Linux test machine would otherwise never match.
func (r *Rewriter) ownedByDaemon(source string) bool {
	// Both readings of the source: as typed, and as a shell may have rewritten
	// it. The second is a candidate only, and the workspace declaring it is
	// what makes it credible.
	spellings := []string{source}
	if r.PosixSource != nil {
		if posix := r.PosixSource(source); posix != "" {
			spellings = append(spellings, posix)
		}
	}

	for _, spelling := range spellings {
		clean := path.Clean(strings.ReplaceAll(spelling, `\`, "/"))
		for _, owned := range r.DaemonPaths {
			owned = path.Clean(owned)
			if clean == owned || strings.HasPrefix(clean, owned+"/") {
				return !r.localExists(source)
			}
		}
	}
	return false
}

func (r *Rewriter) localExists(p string) bool {
	if r.LocalExists != nil {
		return r.LocalExists(p)
	}
	_, err := os.Stat(p)
	return err == nil
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

	changed, hostChanged := false, false
	var hostConfig map[string]json.RawMessage
	var requested workspace.RequestedPorts

	// No HostConfig means no binds and no ports, and the labels below may
	// still change the payload.
	if hostConfigRaw, ok := payload["HostConfig"]; ok {
		if err := json.Unmarshal(hostConfigRaw, &hostConfig); err != nil {
			return nil, fmt.Errorf("rewrite: decoding HostConfig: %w", err)
		}
		// Binds first: a single-file bind cannot be expressed as a bind
		// string, so it leaves that list and arrives in Mounts (ADR 0039).
		moved, err := r.rewriteBinds(ctx, hostConfig, &hostChanged)
		if err != nil {
			return nil, err
		}
		if err := r.rewriteMounts(ctx, hostConfig, moved, &hostChanged); err != nil {
			return nil, err
		}

		if requested, err = r.rewritePorts(hostConfig, &hostChanged); err != nil {
			return nil, err
		}
	}

	// After the ports pass, which decides what the ports label says.
	if err := r.label(payload, requested, &changed); err != nil {
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

// ownerLabels are the marks on everything this client creates: whose it is, and
// which of that account's machines made it.
//
// One function because both the container path and the volume path stamp them,
// and a writer that forgets the client label produces something no collector
// can attribute to a machine (ADR 0029).
func (r *Rewriter) ownerLabels() map[string]string {
	labels := map[string]string{}
	if r.Owner != "" {
		labels[OwnerLabel] = r.Owner
	}
	// Independently of the owner: a volume marked with the machine and not the
	// account is still attributable to a machine, which is what the collector
	// asks about second.
	if r.Client != "" {
		labels[ClientLabel] = r.Client
	}
	return labels
}

// label stamps what this client marks a container with, preserving any label
// the caller set.
//
// The owner says whose container it is and the client says which of that
// account's machines started it; only the second can tell one machine's
// containers from the other's, which is what decides whether a connection may
// be released. The ports label says which local port each publication was
// asked for (ADR 0008).
func (r *Rewriter) label(payload map[string]json.RawMessage, requested workspace.RequestedPorts, changed *bool) error {
	want := r.ownerLabels()
	if ports := requested.String(); ports != "" {
		want[PortsLabel] = ports
	}
	if len(want) == 0 {
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

	stale := false
	for k, v := range want {
		if labels[k] != v {
			stale = true
			break
		}
	}
	if !stale {
		return nil
	}
	for k, v := range want {
		labels[k] = v
	}

	encoded, err := json.Marshal(labels)
	if err != nil {
		return fmt.Errorf("rewrite: encoding labels: %w", err)
	}
	payload["Labels"] = encoded
	*changed = true
	return nil
}

// rewriteBinds handles HostConfig.Binds, the `-v` form.
//
// A bind whose source is a single FILE leaves this list: a bind string has no
// field for a volume subpath, and the subpath is what makes the container see a
// file rather than a directory (ADR 0039). Those are returned for the Mounts
// pass to append, and removed here in the same walk, because the daemon rejects
// the same target appearing in both lists.
func (r *Rewriter) rewriteBinds(ctx context.Context, hostConfig map[string]json.RawMessage, changed *bool) ([]map[string]json.RawMessage, error) {
	raw, ok := hostConfig["Binds"]
	if !ok || string(raw) == "null" {
		return nil, nil
	}

	var binds []string
	if err := json.Unmarshal(raw, &binds); err != nil {
		return nil, fmt.Errorf("rewrite: decoding Binds: %w", err)
	}

	kept := make([]string, 0, len(binds))
	var moved []map[string]json.RawMessage

	for _, spec := range binds {
		parsed, err := ParseBind(spec)
		if err != nil {
			// Not something we understand. Forward it and let the daemon
			// produce its own error, which will be about the actual problem.
			kept = append(kept, spec)
			continue
		}
		if !IsLocalPath(parsed.Source) {
			// A named volume. Left alone: rewriting one would replace the
			// user's persistent data with an export of a directory that does
			// not exist.
			kept = append(kept, spec)
			continue
		}
		if r.ownedByDaemon(parsed.Source) {
			// Untouched, which is also how every option on it survives.
			kept = append(kept, spec)
			continue
		}

		volume, file, err := r.volumeFor(ctx, parsed.Source)
		if err != nil {
			return nil, err
		}
		if file != "" {
			mount, err := fileMount(volume, file, parsed)
			if err != nil {
				return nil, err
			}
			moved = append(moved, mount)
			*changed = true
			continue
		}
		parsed.Source = volume
		kept = append(kept, parsed.String())
		*changed = true
	}

	if !*changed {
		return nil, nil
	}
	encoded, err := json.Marshal(kept)
	if err != nil {
		return nil, fmt.Errorf("rewrite: encoding Binds: %w", err)
	}
	hostConfig["Binds"] = encoded
	return moved, nil
}

// fileMount renders a single-file bind as the volume mount that replaces it.
//
// Typed rather than assembled as generic JSON, unlike everything the caller
// sent: nothing here comes from them but the target and the options, so there
// are no unknown fields to preserve.
//
// The options field is the reason this can fail. A rewritten mount keeps every
// option it arrived with -- `ro` above all, since the export behind it is
// read-write -- and a bind option has no general translation to a volume mount,
// so anything beyond ro/rw is refused by name rather than dropped.
func fileMount(volume, file string, bind BindSpec) (map[string]json.RawMessage, error) {
	readOnly := false
	for _, opt := range strings.Split(bind.Options, ",") {
		switch strings.TrimSpace(opt) {
		case "", "rw":
		case "ro":
			readOnly = true
		default:
			return nil, fmt.Errorf("rewrite: mounting the single file %s with option %q is not supported%s",
				bind.Source, opt, fixMountTheDirectory)
		}
	}

	encoded, err := json.Marshal(struct {
		Type          string
		Source        string
		Target        string
		ReadOnly      bool
		VolumeOptions struct{ Subpath string }
	}{
		Type:          "volume",
		Source:        volume,
		Target:        bind.Target,
		ReadOnly:      readOnly,
		VolumeOptions: struct{ Subpath string }{Subpath: file},
	})
	if err != nil {
		return nil, fmt.Errorf("rewrite: encoding the mount for %s: %w", bind.Source, err)
	}

	var mount map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &mount); err != nil {
		return nil, fmt.Errorf("rewrite: encoding the mount for %s: %w", bind.Source, err)
	}
	return mount, nil
}

// setSubpath puts the exported file's name in the mount's VolumeOptions,
// keeping whatever was already there: a caller may have set NoCopy or Labels,
// and replacing the object would drop them.
func setSubpath(mount map[string]json.RawMessage, file string) error {
	options := map[string]json.RawMessage{}
	if raw, ok := mount["VolumeOptions"]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &options); err != nil {
			return fmt.Errorf("rewrite: decoding VolumeOptions: %w", err)
		}
	}
	encodedFile, err := json.Marshal(file)
	if err != nil {
		return fmt.Errorf("rewrite: encoding subpath: %w", err)
	}
	options["Subpath"] = encodedFile

	encoded, err := json.Marshal(options)
	if err != nil {
		return fmt.Errorf("rewrite: encoding VolumeOptions: %w", err)
	}
	mount["VolumeOptions"] = encoded
	return nil
}

// rewriteMounts handles HostConfig.Mounts, the `--mount` form that Compose and
// the API-level clients prefer.
func (r *Rewriter) rewriteMounts(ctx context.Context, hostConfig map[string]json.RawMessage, moved []map[string]json.RawMessage, changed *bool) error {
	raw, ok := hostConfig["Mounts"]
	if (!ok || string(raw) == "null") && len(moved) == 0 {
		return nil
	}

	// Each mount stays generic for the same reason the envelope does: a mount
	// carries BindOptions, VolumeOptions, TmpfsOptions and Consistency, and
	// dropping any of them changes the mount.
	var mounts []map[string]json.RawMessage
	if ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &mounts); err != nil {
			return fmt.Errorf("rewrite: decoding Mounts: %w", err)
		}
	}

	touched := len(moved) > 0
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
		if !IsLocalPath(source) || r.ownedByDaemon(source) {
			continue
		}

		volume, file, err := r.volumeFor(ctx, source)
		if err != nil {
			return err
		}

		mount["Type"] = json.RawMessage(`"volume"`)
		encodedSource, err := json.Marshal(volume)
		if err != nil {
			return fmt.Errorf("rewrite: encoding mount source: %w", err)
		}
		mount["Source"] = encodedSource

		// A file is exported as a directory holding only itself, so the mount
		// names it as a subpath and the container sees the file (ADR 0039).
		if file != "" {
			if err := setSubpath(mount, file); err != nil {
				return err
			}
		}

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
	mounts = append(mounts, moved...)
	encoded, err := json.Marshal(mounts)
	if err != nil {
		return fmt.Errorf("rewrite: encoding Mounts: %w", err)
	}
	hostConfig["Mounts"] = encoded
	*changed = true
	return nil
}

// fixMountTheDirectory is the remedy for every way a single file can be
// refused, so the wording lives in one place.
const fixMountTheDirectory = "\n\tfix: mount the directory containing it instead"

// minSubpathMajor is the first Docker release carrying VolumeOptions.Subpath,
// which is API v1.45. Without it a single-file bind cannot be expressed at all
// (ADR 0039).
const minSubpathMajor = 26

// supportsSubpath reads the workspace's reported Docker version.
//
// Unknown means yes: the version is a string from another machine, and refusing
// a working setup because it was not in the expected shape is worse than
// letting the daemon answer for itself. It reports "unavailable" when its own
// daemon is down, and that path already fails elsewhere with a better message.
func supportsSubpath(version string) bool {
	major, _, _ := strings.Cut(version, ".")
	n, err := strconv.Atoi(strings.TrimSpace(major))
	if err != nil {
		return true
	}
	return n >= minSubpathMajor
}

// volumeFor exports a local directory and returns the name of the volume
// backing it on the workspace, creating that volume if needed.
func (r *Rewriter) volumeFor(ctx context.Context, localPath string) (name, file string, err error) {
	// Held across BOTH steps: registering the share is what tells the collector
	// this volume is spoken for, and the volume does not exist until the step
	// after it.
	defer r.Guard.hold()()

	exportPath, file, err := r.Shares.Share(localPath)
	if err != nil {
		return "", "", fmt.Errorf("rewrite: exporting %s: %w", localPath, err)
	}
	if file != "" && !supportsSubpath(r.DockerVersion) {
		return "", "", fmt.Errorf(
			"rewrite: mounting the single file %s needs Docker %d or newer on the workspace, which reports %s%s",
			localPath, minSubpathMajor, r.DockerVersion, fixMountTheDirectory)
	}

	name, err = workspace.VolumeNameForExport(r.Client, exportPath)
	if err != nil {
		return "", "", fmt.Errorf("rewrite: %w", err)
	}

	opts := workspace.NFSVolumeOptions(r.NFSPort, exportPath)
	labels := r.ownerLabels()
	labels[ManagedLabel] = ManagedShare
	if err := r.Volumes.EnsureVolume(ctx, name, opts, labels); err != nil {
		return "", "", fmt.Errorf("rewrite: creating volume for %s: %w", localPath, err)
	}
	return name, file, nil
}
