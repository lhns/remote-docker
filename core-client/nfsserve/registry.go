package nfsserve

import (
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/osfs"

	"github.com/lhns/remote-docker/core/workspace"
)

// Share is one local directory exposed to the workspace, or one file.
type Share struct {
	// ExportPath is how the workspace addresses it: "/m/<id>".
	ExportPath string

	// LocalPath is the directory or file on this machine, in its original
	// spelling.
	LocalPath string

	// File is the base name when LocalPath is a single file, and empty for a
	// directory. The export is a synthesised directory holding exactly that
	// name, and the mount carries it as a volume subpath so the container sees
	// a file at the target (ADR 0039).
	File string

	// Words are what this share was registered with, resolved: the owner is
	// always set. Fixed for the life of the share, which is also why they
	// can be compared: a share's filesystem is built with them and never
	// replaced (ADR 0046).
	Words workspace.ShareWords

	fs billy.Filesystem
}

// Registry holds the set of local directories currently exported.
//
// Registration is lazy: a directory is added the first time a bind mount names
// it. The workspace's view of this machine is therefore exactly the set of
// paths the user asked for, and nothing else is reachable, which is the
// property that lets a single export serve arbitrary local paths without
// exposing the filesystem root. See ADR 0007.
type Registry struct {
	attrs Attrs

	// Restore is consulted when a mount names an export this registry does not
	// hold, and returns the local directory that export stands for.
	//
	// It exists because registration is per process while the VOLUME naming an
	// export outlives it. `docker compose up -d` on containers that already
	// exist only starts them, so no /containers/create arrives, so nothing
	// registers the share, while dockerd still mounts the volume created last
	// time and is told there is no such file or directory. Recreating the
	// containers was the only way back.
	//
	// The words come back with the path, so a restored share reports the
	// owner it was first given rather than the session's default.
	//
	// Nil means a miss is a miss, which is what a query session and every test
	// without a record want. What it must NOT be is a way for the far side to
	// name a directory: see session.shareStore, where the answer comes from
	// what this machine wrote down and the id is recomputed from the path
	// before it is believed.
	Restore func(exportPath string) (localPath string, words workspace.ShareWords, ok bool)

	// Recorded names the exports Restore can answer for. A root handle carries
	// only a digest of its export, and this is how one is matched to a record.
	Recorded func() []string

	// OnRead is told every read the workspace makes through a share, in
	// bytes, as it happens. On a share with a cache that is exactly the
	// stream of misses, which is what a prefetch policy runs on (ADR 0045).
	// Nil reports nothing and costs nothing. Set before the first share is
	// registered: a share's filesystem is built with it.
	OnRead ReadObserver

	// Log is where a share's own diagnostics go, the wire having no room for
	// them. Read when a share's filesystem is built, so set it before the
	// first share is registered.
	Log *slog.Logger

	// The tracing threshold, read once. shareFS runs on every registration, and a
	// value that is not understood is worth saying once rather than once per
	// share.
	traceOnce sync.Once
	trace     time.Duration

	// Neither map ever shrinks and there is no unregister, deliberately: a
	// share removed leaves every container mounting it with `Stale file
	// handle` against a mount that still looks fine. The reasoning is the
	// "share registry never shrinks" invariant in CLAUDE.md.
	mu     sync.RWMutex
	shares map[string]*Share // keyed by export path
	byPath map[string]*Share // keyed by canonical local path
}

// NewRegistry returns an empty registry reporting the given attributes.
func NewRegistry(attrs Attrs) *Registry {
	return &Registry{
		attrs:  attrs,
		shares: map[string]*Share{},
		byPath: map[string]*Share{},
	}
}

// Register exports localPath at /m/<id>, deriving the id from the path so it
// is the same on every run. A directory already registered is returned as it
// stands rather than duplicated, with ITS words: the caller compares them to
// what it asked for.
//
// Unset words take the registry's attributes. They are applied to a copy of
// them, because r.attrs is the session's (it carries AlwaysExecutable on
// Windows) and nfsserve.DefaultAttrs is not.
func (r *Registry) Register(localPath string, words workspace.ShareWords) (*Share, error) {
	info, err := os.Stat(localPath)
	if err != nil {
		return nil, fmt.Errorf("nfsserve: cannot export %s: %w", localPath, err)
	}
	// A directory is exported as itself; a regular file as a synthesised
	// directory holding only that file (ADR 0039). Anything else cannot be
	// served: a socket, a device or a FIFO is a kernel object reached through
	// a path, not content, so what crosses NFS is the name and nothing behind
	// it. That is equally true of one sitting inside a directory this registry
	// already exports, which is why the message says so.
	if !info.IsDir() && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("nfsserve: %s is a %s, and that cannot be reached through a file share",
			localPath, describeMode(info.Mode()))
	}

	key := workspace.CanonicalKey(localPath)

	r.mu.Lock()
	defer r.mu.Unlock()

	if existing, ok := r.byPath[key]; ok {
		return existing, nil
	}
	exportPath := workspace.ExportPathForID(workspace.ShareID(localPath))

	// WithBoundOS keeps every operation inside baseDir. It is the boundary
	// that stops a crafted path escaping a share, and it is why each share
	// gets its own filesystem instead of one rooted higher up.
	//
	// For a file the bound directory is the one containing it, and singleFileFS
	// is what stops the siblings in there being reachable.
	base, file := localPath, ""
	if !info.IsDir() {
		// Dir and Base rather than trimming a split: they are what keep a file
		// sitting at a root ("/x.conf", "C:\x.conf") pointing at that root
		// instead of at an empty path.
		base, file = filepath.Dir(localPath), filepath.Base(localPath)
	}

	attrs := r.attrs
	if words.Owner.Set {
		attrs.UID, attrs.GID = words.Owner.UID, words.Owner.GID
	}
	words.Owner = workspace.Owner{UID: attrs.UID, GID: attrs.GID, Set: true}

	share := &Share{
		ExportPath: exportPath,
		LocalPath:  localPath,
		File:       file,
		Words:      words,
		fs:         withAttrs(r.shareFS(base, file), attrs, exportPath, r.OnRead),
	}
	r.shares[exportPath] = share
	r.byPath[key] = share
	return share, nil
}

// Lookup finds the share serving an export path. A path below a share's root
// resolves to that share, so mounting a subdirectory works.
func (r *Registry) Lookup(exportPath string) (*Share, string, bool) {
	clean := normalizeExport(exportPath)

	r.mu.RLock()
	defer r.mu.RUnlock()

	if share, ok := r.shares[clean]; ok {
		return share, "/", true
	}
	for export, share := range r.shares {
		if rest, ok := strings.CutPrefix(clean, export+"/"); ok {
			return share, "/" + rest, true
		}
	}
	return nil, "", false
}

// LookupOrRestore is Lookup, with one chance to bring a share back.
//
// Separate from Lookup on purpose: only a MOUNT, or a root handle through
// restoreMatching, may resurrect a share. Shares, which the volume collector
// and the file watcher both read, has to keep answering with what is exported
// right now, and a lookup that quietly registered things would make "in use"
// depend on who asked.
func (r *Registry) LookupOrRestore(exportPath string) (*Share, string, bool) {
	if share, rest, ok := r.Lookup(exportPath); ok {
		return share, rest, true
	}
	if r.Restore == nil {
		return nil, "", false
	}

	// Only the export itself, never a subdirectory of one. A mount of
	// /m/<id>/sub can only follow a mount of /m/<id>, and restoring from a
	// deeper path would mean deriving the share from something the far side
	// composed. Which is also why the rest is "/" without asking: a restore
	// registers exactly the path it was given.
	clean := normalizeExport(exportPath)
	local, words, ok := r.Restore(clean)
	if !ok {
		return nil, "", false
	}
	share, err := r.Register(local, words)
	if err != nil || share.ExportPath != clean {
		return nil, "", false
	}
	return share, "/", true
}

// restoreMatching restores the recorded export match accepts. A kernel
// presents a root handle only for an export it has mounted, so this brings
// back only what a container is using (ADR 0027).
func (r *Registry) restoreMatching(match func(export string) bool) (*Share, bool) {
	if r.Recorded == nil {
		return nil, false
	}
	for _, export := range r.Recorded() {
		if match(export) {
			share, rest, ok := r.LookupOrRestore(export)
			return share, ok && rest == "/"
		}
	}
	return nil, false
}

// Shares returns every registered share, ordered by export path.
func (r *Registry) Shares() []*Share {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]*Share, 0, len(r.shares))
	for _, s := range r.shares {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ExportPath < out[j].ExportPath })
	return out
}

// normalizeExport cleans a mount path from the wire into the form the registry
// keys on. Clean resolves any ".." the client sent, so a path cannot address a
// share it was not given.
func normalizeExport(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	// Clean leaves a trailing slash only on the root, and trimming that would
	// turn "/" into the empty string, which matches no share but also
	// matches no branch below, so keep it whole.
	if p = path.Clean(p); p == "/" {
		return "/"
	}
	return strings.TrimSuffix(p, "/")
}

// shareFS is a share's filesystem before attributes. The ONE place this is
// built.
//
// The order is the whole of it. From the disk up: a bound osfs at base;
// noFollowFS, which has to sit directly on it so that every layer above
// removes and renames a link as a link; the descriptor cache (fdcache.go, on
// unless REMOTE_DOCKER_NFS_FDCACHE turns it off) and the tracer (trace.go, off
// unless REMOTE_DOCKER_NFS_TRACE asks for it), neither of which is present at
// all when its switch says no, because each costs something on every call; and,
// for a single-file share, the one-file view on top (ADR 0039).
func (r *Registry) shareFS(base, file string) billy.Filesystem {
	var inner billy.Filesystem = &noFollowFS{
		Filesystem: osfs.New(base, osfs.WithBoundOS()),
		log:        r.Log,
	}
	if idle := fdCacheIdle(); idle > 0 {
		inner = withFDCache(inner, idle, fdCacheMax)
	}
	r.traceOnce.Do(func() { r.trace = traceThreshold(r.Log) })
	if r.trace > 0 {
		inner = withTrace(inner, base, r.Log, r.trace)
	}
	if file != "" {
		return &singleFileFS{Filesystem: inner, name: file}
	}
	return inner
}
