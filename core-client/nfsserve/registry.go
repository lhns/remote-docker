package nfsserve

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/osfs"

	"github.com/lhns/remote-docker/core/workspace"
)

// Share is one local directory exposed to the workspace, or one file.
type Share struct {
	// ExportPath is how the workspace addresses it: "/cwd" or "/m/<id>".
	ExportPath string

	// LocalPath is the directory or file on this machine, in its original
	// spelling.
	LocalPath string

	// File is the base name when LocalPath is a single file, and empty for a
	// directory. The export is a synthesised directory holding exactly that
	// name, and the mount carries it as a volume subpath so the container sees
	// a file at the target (ADR 0039).
	File string

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
	// Nil means a miss is a miss, which is what a query session and every test
	// without a record want. What it must NOT be is a way for the far side to
	// name a directory: see session.shareStore, where the answer comes from
	// what this machine wrote down and the id is recomputed from the path
	// before it is believed.
	Restore func(exportPath string) (localPath string, ok bool)

	// OnRead is told every read the workspace makes through a share, in
	// bytes, as it happens. On a share with a cache that is exactly the
	// stream of misses, which is what a prefetch policy runs on (ADR 0045).
	// Nil reports nothing and costs nothing. Set before the first share is
	// registered: a share's filesystem is built with it.
	OnRead ReadObserver

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

// RegisterCWD exports localPath at /cwd, where the interactive shell lands.
func (r *Registry) RegisterCWD(localPath string) (*Share, error) {
	return r.register(workspace.ExportCWD, localPath)
}

// Register exports localPath at /m/<id>, deriving the id from the path so it
// is the same on every run. A directory already registered is returned as it
// stands rather than duplicated.
func (r *Registry) Register(localPath string) (*Share, error) {
	return r.register("", localPath)
}

func (r *Registry) register(exportPath, localPath string) (*Share, error) {
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

	if existing, ok := r.byPath[key]; ok && (exportPath == "" || existing.ExportPath == exportPath) {
		return existing, nil
	}
	if exportPath == "" {
		exportPath = workspace.ExportPathForID(workspace.ShareID(localPath))
	}

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

	share := &Share{
		ExportPath: exportPath,
		LocalPath:  localPath,
		File:       file,
		fs:         withAttrs(shareFS(base, file), r.attrs, exportPath, r.OnRead),
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
// Separate from Lookup on purpose: only a MOUNT may resurrect a share. Shares,
// which the volume collector and the file watcher both read, has to keep
// answering with what is exported right now, and a lookup that quietly
// registered things would make "in use" depend on who asked.
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
	local, ok := r.Restore(clean)
	if !ok {
		return nil, "", false
	}
	share, err := r.register(clean, local)
	if err != nil {
		return nil, "", false
	}
	return share, "/", true
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

// SetAttrs changes the attributes reported for shares registered from now on,
// and for existing ones: the workspace account's uid is only known once
// connected, while the working directory is registered before that.
func (r *Registry) SetAttrs(attrs Attrs) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.attrs = attrs
	for _, share := range r.shares {
		// Rebuilt the same way it was registered: a single-file share must
		// be rooted at its directory (ADR 0039), and rooting it at the file
		// leaves a share nothing can mount.
		base := share.LocalPath
		if share.File != "" {
			base = filepath.Dir(share.LocalPath)
		}
		share.fs = withAttrs(shareFS(base, share.File), attrs, share.ExportPath, r.OnRead)
	}
}

// shareFS is a share's filesystem before attributes: a bound osfs at base,
// narrowed to one file when the share is one (ADR 0039). The ONE place this is
// built, so registration and SetAttrs cannot disagree about it.
func shareFS(base, file string) billy.Filesystem {
	inner := osfs.New(base, osfs.WithBoundOS())
	if file != "" {
		return &singleFileFS{Filesystem: inner, name: file}
	}
	return inner
}
