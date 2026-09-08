package nfsserve

import (
	"fmt"
	"io"
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

	// Log is where a share's own diagnostics go, the wire having no room for
	// them. Read when a share's filesystem is built, so set it before the
	// first share is registered.
	Log *slog.Logger

	// The tracing threshold, read once. shareFS runs on every registration and
	// again for every share on every SetAttrs, and a value that is not
	// understood is worth saying once rather than once per share per connect.
	traceOnce sync.Once
	trace     time.Duration

	// Neither map ever shrinks and there is no unregister, deliberately. A
	// share's root handle is derived from its export path (ADR 0033) and MOUNT
	// issues it exactly once, so a share removed here leaves every running
	// container with `Stale file handle` against a mount that still looks
	// fine, and nothing on this side can tell which exports still have a live
	// kernel mount. Shares also feeds the idle release and rewrite.Guard,
	// where "in use" must not depend on who asked. Bounded by design: one
	// entry per distinct path this process has exported, tens of bytes each,
	// for the life of the client process. What a share HOLDS is released
	// instead, by closeFS.
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
		fs:         withAttrs(r.shareFS(base, file), r.attrs, exportPath, r.OnRead),
	}
	// A registration of a path already held returns that share above, so the
	// only way to build a second stack for one export is to register a
	// DIFFERENT directory at an export path already taken. That share stops
	// being reachable, so what it holds is given up and its own byPath entry
	// goes with it, rather than pointing at a filesystem nothing serves.
	if old, ok := r.shares[exportPath]; ok {
		delete(r.byPath, workspace.CanonicalKey(old.LocalPath))
		closeFS(old.fs)
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
		outgoing := share.fs
		share.fs = withAttrs(r.shareFS(base, share.File), attrs, share.ExportPath, r.OnRead)
		closeFS(outgoing)
	}
}

// closeFS gives up what a share's filesystem holds, which today is the
// descriptor cache and nothing else.
//
// It walks the stack rather than asserting io.Closer on the top of it: every
// wrapper embeds billy.Filesystem as an INTERFACE, so a Close on a layer below
// is not promoted through it, and both optional layers are absent entirely
// when their switch says no. Unwrapping by concrete type is therefore what
// tolerates a missing layer, and the stack is built one function above.
func closeFS(fs billy.Filesystem) {
	for {
		switch v := fs.(type) {
		case io.Closer:
			_ = v.Close()
			return
		case *attrFS:
			fs = v.Filesystem
		case *singleFileFS:
			fs = v.Filesystem
		case *traceFS:
			fs = v.Filesystem
		default:
			return
		}
	}
}

// shareFS is a share's filesystem before attributes. The ONE place this is
// built, so registration and SetAttrs cannot disagree about it.
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
