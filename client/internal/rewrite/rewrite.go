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

// Guard keeps garbage collection off a volume a rewrite is creating. The
// daemon calls it in use only once a container names it, and a collected one
// is silently recreated EMPTY. Exported answers what the daemon cannot; the
// lock spans registering the share and creating its volume.
type Guard struct {
	mu sync.Mutex

	// Exported reports whether a volume backs an exported directory. Nil means
	// nothing is exported.
	Exported func(volume string) bool
}

// hold locks the guard and returns its release. A nil guard is a no-op.
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
type Sharer interface {
	// Share exports localPath and returns its export path, e.g. "/m/<id>",
	// and for a single file the base name the mount uses as a subpath
	// (ADR 0039).
	Share(localPath string) (exportPath, file string, err error)
}

// VolumeEnsurer creates a volume on the workspace daemon if it is not already
// there.
type VolumeEnsurer interface {
	EnsureVolume(ctx context.Context, name string, driverOpts, labels map[string]string) error
}

// Cache is the workspace's end of a share with a union (ADR 0044). Prepare
// mounts the union and answers with the path the container binds.
type Cache interface {
	Prepare(ctx context.Context, export, cache string, port int, mode workspace.Mode) (string, error)

	// Attach registers the share and returns AT ONCE; any prefetch runs in the
	// background, since a miss falls through to the live export.
	Attach(export, localPath string, mode workspace.Mode)
}

// Rewriter converts bind mounts naming local paths into NFS-backed volumes.
type Rewriter struct {
	Shares  Sharer
	Volumes VolumeEnsurer

	// NFSPort is the workspace loopback port where the reverse tunnel exposes
	// this client's NFS server.
	NFSPort int

	// NConnect is the mount's nconnect; zero, the default, emits nothing. See
	// NConnectEnv.
	NConnect int

	// Owner labels this account's containers. Empty disables labelling.
	Owner string

	// Client identifies THIS MACHINE; volumes are named and labelled per client
	// (ADR 0029). Empty gives the unqualified names older clients made.
	Client string

	// Mode fills each axis a mount left unset; ModePaths overrides it for a
	// directory and everything under it (ADR 0042).
	Mode      workspace.Mode
	ModePaths map[string]workspace.Mode

	// OpenCache reaches the cache channel, opened only once a mount needs it.
	// Its error is the tail of the refusal. Nil means no cache at all.
	OpenCache func(ctx context.Context) (Cache, error)

	// UnionReady is workspace.Info.Union; a mode it cannot serve is refused
	// before anything is created.
	UnionReady string

	// Watching says whether local changes are replayed. `read=cached` and every
	// union need it, and are refused without it rather than served stale.
	Watching bool

	// Guard is shared with the Collector.
	Guard *Guard

	// LocalPortFree reports whether this machine can open a port (see
	// rewritePorts). Nil skips the check.
	LocalPortFree func(port int) error

	// DockerVersion is the workspace daemon's version, read only to decide
	// whether a single file can be mounted (ADR 0039). Empty means capable.
	DockerVersion string

	// DaemonPaths are paths the workspace daemon resolves itself (ADR 0041);
	// a bind naming one passes through untouched.
	DaemonPaths []string

	// LocalExists reports whether a path is on THIS machine. Nil means os.Stat.
	LocalExists func(path string) bool

	// PosixSource returns the POSIX path a shell (Git Bash) may have rewritten
	// a bind source from, or "" (ADR 0040). Nil disables it.
	PosixSource func(source string) string
}

// ownedByDaemon reports whether the workspace resolves this source itself.
// Only for a source this machine lacks, so a typo still fails.
func (r *Rewriter) ownedByDaemon(source string) bool {
	if r.declared(source) {
		return !r.localExists(source)
	}
	// A candidate only; the workspace declaring it is what makes it credible.
	if r.PosixSource != nil && r.declared(r.PosixSource(source)) {
		return !r.localExists(source)
	}
	return false
}

// declared reports whether the workspace named this path or one above it.
// POSIX rules by hand, not filepath or CanonicalKey, which follow THIS host's.
func (r *Rewriter) declared(source string) bool {
	if source == "" {
		return false
	}
	clean := path.Clean(strings.ReplaceAll(source, `\`, "/"))
	for _, owned := range r.DaemonPaths {
		owned = path.Clean(owned)
		if clean == owned || strings.HasPrefix(clean, owned+"/") {
			return true
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

// ContainerCreate rewrites the body of POST /containers/create. Generic JSON,
// never typed structs, so fields we do not know survive.
func (r *Rewriter) ContainerCreate(ctx context.Context, body []byte) ([]byte, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("rewrite: decoding container create: %w", err)
	}

	changed, hostChanged := false, false
	var hostConfig map[string]json.RawMessage
	var requested workspace.RequestedPorts

	// A directory in both mount lists is one share: the second sees the
	// first's mode.
	modes := map[string]workspace.Mode{}

	if hostConfigRaw, ok := payload["HostConfig"]; ok {
		if err := json.Unmarshal(hostConfigRaw, &hostConfig); err != nil {
			return nil, fmt.Errorf("rewrite: decoding HostConfig: %w", err)
		}
		// Binds first: a single-file bind cannot be expressed as a bind
		// string, so it leaves that list and arrives in Mounts (ADR 0039).
		moved, err := r.rewriteBinds(ctx, modes, hostConfig, &hostChanged)
		if err != nil {
			return nil, err
		}
		if err := r.rewriteMounts(ctx, modes, hostConfig, moved, &hostChanged); err != nil {
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

// ownerLabels mark everything this client creates with its account and
// machine. Shared by containers and volumes, so neither forgets the client
// label (ADR 0029).
func (r *Rewriter) ownerLabels() map[string]string {
	labels := map[string]string{}
	if r.Owner != "" {
		labels[workspace.OwnerLabel] = r.Owner
	}
	if r.Client != "" {
		labels[workspace.ClientLabel] = r.Client
	}
	return labels
}

// label stamps the owner labels and the ports label (ADR 0008) on a container,
// preserving the caller's own.
func (r *Rewriter) label(payload map[string]json.RawMessage, requested workspace.RequestedPorts, changed *bool) error {
	want := r.ownerLabels()
	if ports := requested.String(); ports != "" {
		want[workspace.PortsLabel] = ports
	}
	if len(want) == 0 {
		return nil
	}

	labels := map[string]string{}
	if raw, ok := payload["Labels"]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &labels); err != nil {
			// Left alone; the daemon reports anything malformed.
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

// rewriteBinds handles HostConfig.Binds, the `-v` form. A single-FILE bind
// needs a subpath, which a bind string cannot carry, so it is moved to Mounts
// in the same walk: the daemon rejects one target in both lists (ADR 0039).
func (r *Rewriter) rewriteBinds(ctx context.Context, modes map[string]workspace.Mode, hostConfig map[string]json.RawMessage, changed *bool) ([]map[string]json.RawMessage, error) {
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
			// Forwarded for the daemon to report.
			kept = append(kept, spec)
			continue
		}
		// Never rewrite a named volume (it is the user's data) or a path the
		// workspace owns. Either keeps every option but ours.
		if !IsLocalPath(parsed.Source) || r.ownedByDaemon(parsed.Source) {
			options, err := withoutOurWords(parsed.Options)
			if err != nil {
				return nil, err
			}
			if options != parsed.Options {
				parsed.Options = options
				spec = parsed.String()
				*changed = true
			}
			kept = append(kept, spec)
			continue
		}

		// Mode words are consumed, never forwarded: the daemon rejects them.
		// Every other option, `ro` above all, stays verbatim.
		asked, spelled, options, err := splitMode(parsed.Options)
		if err != nil {
			return nil, err
		}
		parsed.Options = options
		mode, err := r.resolveMode(ctx, modes, parsed.Source, asked, spelled)
		if err != nil {
			return nil, err
		}

		volume, file, err := r.volumeFor(ctx, parsed.Source, mode)
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

// fileMount renders a single-file bind as a volume mount. Typed, since it has
// no caller fields to preserve. `ro` must survive (the export is read-write),
// and any option beyond ro/rw is refused rather than dropped.
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

// setSubpath sets VolumeOptions.Subpath, keeping the caller's other options.
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
func (r *Rewriter) rewriteMounts(ctx context.Context, modes map[string]workspace.Mode, hostConfig map[string]json.RawMessage, moved []map[string]json.RawMessage, changed *bool) error {
	raw, ok := hostConfig["Mounts"]
	if (!ok || string(raw) == "null") && len(moved) == 0 {
		return nil
	}

	// Generic maps, so every option (ReadOnly above all) survives.
	var mounts []map[string]json.RawMessage
	if ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &mounts); err != nil {
			return fmt.Errorf("rewrite: decoding Mounts: %w", err)
		}
	}

	touched := len(moved) > 0
	for _, mount := range mounts {
		var mountType, source string
		if err := json.Unmarshal(mount["Type"], &mountType); err != nil {
			continue
		}
		// volume, tmpfs, npipe and cluster name no path on this machine.
		if mountType == "bind" {
			if err := json.Unmarshal(mount["Source"], &source); err != nil {
				continue
			}
		}
		if mountType != "bind" || !IsLocalPath(source) || r.ownedByDaemon(source) {
			dropped, err := dropOurConsistency(mount)
			if err != nil {
				return err
			}
			touched = touched || dropped
			continue
		}

		asked, spelled, err := takeMode(mount)
		if err != nil {
			return err
		}
		mode, err := r.resolveMode(ctx, modes, source, asked, spelled)
		if err != nil {
			return err
		}

		volume, file, err := r.volumeFor(ctx, source, mode)
		if err != nil {
			return err
		}

		// A union is a path, so it is BOUND; as a volume name Docker would
		// create an empty volume by that name.
		if mode.Union() {
			mount["Type"] = json.RawMessage(`"bind"`)
		} else {
			mount["Type"] = json.RawMessage(`"volume"`)
		}
		encodedSource, err := json.Marshal(volume)
		if err != nil {
			return fmt.Errorf("rewrite: encoding mount source: %w", err)
		}
		mount["Source"] = encodedSource

		// A single file is mounted by subpath (ADR 0039).
		if file != "" {
			if err := setSubpath(mount, file); err != nil {
				return err
			}
		}

		// The one deliberate deletion: the daemon rejects BindOptions on a
		// volume mount.
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

// fixMountTheDirectory is the remedy for every single-file refusal.
const fixMountTheDirectory = "\n  fix: mount the directory containing it instead"

// minSubpathMajor is the first Docker with VolumeOptions.Subpath (API v1.45).
const minSubpathMajor = 26

// supportsSubpath reads the workspace's Docker version. Unparseable means yes:
// the daemon then answers for itself.
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
func (r *Rewriter) volumeFor(ctx context.Context, localPath string, mode workspace.Mode) (name, file string, err error) {
	// Held across registering the share AND creating its volume.
	defer r.Guard.hold()()

	exportPath, file, err := r.Shares.Share(localPath)
	if err != nil {
		return "", "", fmt.Errorf("rewrite: exporting %s: %w", localPath, err)
	}
	if file != "" && mode.Union() {
		// A union is bound by path and a file is mounted by subpath, and the
		// two do not combine: the result is a volume named after a path, or
		// a bind carrying VolumeOptions, and the daemon refuses both.
		return "", "", fmt.Errorf("rewrite: mounting the single file %s with write=%s is not supported%s",
			localPath, mode.Write, fixMountTheDirectory)
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

	labels := r.ownerLabels()
	labels[workspace.ManagedLabel] = workspace.ManagedShare

	if mode.Union() {
		// A union path the caller binds, not a volume (ADR 0044).
		merged, err := r.union(ctx, exportPath, name, localPath, labels, mode)
		if err != nil {
			return "", "", err
		}
		return merged, file, nil
	}

	opts := workspace.NFSVolumeOptions(r.NFSPort, exportPath, mode.Read, r.NConnect)
	if err := r.Volumes.EnsureVolume(ctx, name, opts, labels); err != nil {
		return "", "", fmt.Errorf("rewrite: creating volume for %s: %w", localPath, err)
	}
	return name, file, nil
}

// union creates the cache volume, prepares the union over the live export,
// attaches the cache, and answers with the path the container binds
// (ADR 0044).
func (r *Rewriter) union(ctx context.Context, export, share, localPath string, labels map[string]string, mode workspace.Mode) (string, error) {
	cache := workspace.CacheVolumeName(share)

	// Empty map, not nil: JSON null is not an object.
	if err := r.Volumes.EnsureVolume(ctx, cache, map[string]string{}, labels); err != nil {
		return "", fmt.Errorf("rewrite: creating the cache for %s: %w", localPath, err)
	}

	// Memoised: resolveMode already opened it.
	channel, err := r.openCache(ctx)
	if err != nil {
		return "", fmt.Errorf("rewrite: reaching the workspace's cache for %s: %w", localPath, err)
	}

	merged, err := channel.Prepare(ctx, export, cache, r.NFSPort, mode)
	if err != nil {
		return "", fmt.Errorf("rewrite: preparing the cache for %s: %w", localPath, err)
	}

	// Asynchronous: the container starts against an empty cache.
	channel.Attach(export, localPath, mode)

	return merged, nil
}
