package nfsserve

import (
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/osfs"

	"github.com/lhns/remote-docker/pkg/workspace"
)

// Share is one local directory exposed to the workspace.
type Share struct {
	// ExportPath is how the workspace addresses it: "/cwd" or "/m/<id>".
	ExportPath string

	// LocalPath is the directory on this machine, in its original spelling.
	LocalPath string

	fs billy.Filesystem
}

// Registry holds the set of local directories currently exported.
//
// Registration is lazy: a directory is added the first time a bind mount names
// it. The workspace's view of this machine is therefore exactly the set of
// paths the user asked for, and nothing else is reachable -- which is the
// property that lets a single export serve arbitrary local paths without
// exposing the filesystem root. See ADR 0007.
type Registry struct {
	attrs Attrs

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
	if !info.IsDir() {
		// A bind mount of a single file is legal in Docker but has no
		// meaningful NFS export, and silently exporting the parent directory
		// would share more than was asked for.
		return nil, fmt.Errorf("nfsserve: %s is not a directory; only directories can be exported", localPath)
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
	inner := osfs.New(localPath, osfs.WithBoundOS())

	share := &Share{
		ExportPath: exportPath,
		LocalPath:  localPath,
		fs:         withAttrs(inner, r.attrs),
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
	// turn "/" into the empty string -- which matches no share but also
	// matches no branch below, so keep it whole.
	if p = path.Clean(p); p == "/" {
		return "/"
	}
	return strings.TrimSuffix(p, "/")
}

// SetAttrs changes the attributes reported for shares registered from now on,
// and for existing ones.
//
// The session needs this because the workspace account's uid is only known
// once connected, while the working directory is registered before that --
// the endpoint has to exist before anything can ask us to connect.
func (r *Registry) SetAttrs(attrs Attrs) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.attrs = attrs
	for _, share := range r.shares {
		share.fs = withAttrs(osfs.New(share.LocalPath, osfs.WithBoundOS()), attrs)
	}
}
