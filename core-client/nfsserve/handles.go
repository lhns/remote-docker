package nfsserve

// A share's root handle is derived from the export path so it outlives this
// process: MOUNT issues it once and the kernel never mounts again, while below
// the root Linux re-looks-up after ESTALE and go-nfs's in-memory handles are
// enough (ADR 0033).

import (
	"crypto/sha256"
	"errors"
	"io/fs"
	"log/slog"

	"github.com/go-git/go-billy/v5"
	nfs "github.com/willscott/go-nfs"
)

// A root handle is the export key followed by the cache's own handle; every
// other handle is the cache's alone, byte for byte.
//
// Told apart by LENGTH. A tag byte on every handle made every mount succeed and
// every read fail with "permission denied", for no reason anyone has found; the
// measurement rules it out. So the cache's handles must stay 16 bytes, which a
// test pins.
const (
	exportKeySize = 8

	// cachedHandleSize is what go-nfs's caching handler produces: a uuid.
	cachedHandleSize = 16

	// rootHandleSize is how a share root is recognised on the way back in.
	rootHandleSize = exportKeySize + cachedHandleSize
)

// rootHandler answers for share roots and delegates everything else.
type rootHandler struct {
	nfs.Handler // the caching handler: verifiers, and every handle but a root

	registry *Registry
	log      *slog.Logger
}

// errStaleExport is what a handle naming a share that is neither exported nor
// recorded gets. Never a guess: a handle names a capability, and resolves only
// to one this machine wrote down (ADR 0027).
var errStaleExport = errors.New("nfsserve: no such export")

func (h *rootHandler) ToHandle(f billy.Filesystem, path []string) []byte {
	cached := h.Handler.ToHandle(f, path)

	// Only the share's own root: a Chroot mount given it would serve the share
	// root instead of the subdirectory asked for.
	export := exportRootOf(f)
	if len(path) != 0 || export == "" || len(cached) != cachedHandleSize {
		return cached
	}
	return append(exportKey(export), cached...)
}

func (h *rootHandler) FromHandle(handle []byte) (billy.Filesystem, []string, error) {
	if len(handle) != rootHandleSize {
		fs, path, err := h.Handler.FromHandle(handle)
		if err != nil {
			// go-nfs answers ESTALE and logs nothing. A path lookup
			// recovers on its own (ADR 0033); an already-open descriptor
			// cannot, so it reaches the application.
			h.log.Warn("nfs: a file handle could not be resolved",
				"bytes", len(handle), "err", err)
		}
		return fs, path, err
	}
	key, cached := handle[:exportKeySize], handle[exportKeySize:]

	// The cache while this process holds it; the derived key after a restart.
	if fs, path, err := h.Handler.FromHandle(cached); err == nil {
		return fs, path, nil
	}
	share, ok := h.shareForKey(key)
	if !ok {
		share, ok = h.registry.restoreMatching(func(export string) bool {
			return string(exportKey(export)) == string(key)
		})
	}
	if !ok {
		return nil, nil, errStaleExport
	}
	return share.fs, []string{}, nil
}

func (h *rootHandler) InvalidateHandle(f billy.Filesystem, handle []byte) error {
	// The derived half is not stored and cannot be forgotten; the cache's half
	// is passed on so it can forget what it knows.
	if len(handle) == rootHandleSize {
		return h.Handler.InvalidateHandle(f, handle[exportKeySize:])
	}
	return h.Handler.InvalidateHandle(f, handle)
}

// shareForKey finds the share whose export path matches a root handle, asking
// the registry every time so a handle reaches only a share exported now.
func (h *rootHandler) shareForKey(key []byte) (*Share, bool) {
	if len(key) != exportKeySize {
		return nil, false
	}
	for _, share := range h.registry.Shares() {
		if string(exportKey(share.ExportPath)) == string(key) {
			return share, true
		}
	}
	return nil, false
}

// exportKey is the part of a root handle that names the share.
func exportKey(export string) []byte {
	sum := sha256.Sum256([]byte(export))
	return sum[:exportKeySize]
}

// mover is go-nfs's optional interface for re-pointing a cached handle at a
// renamed file (nfs_onrename.go's renameHandleMover), satisfied by the caching
// handler.
type mover interface {
	Rename(billy.Filesystem, []string, billy.Filesystem, []string) error
}

// Rename re-points a cached handle at the file's new path, so a client that
// renames a file it holds open keeps its handle; embedding does not carry the
// optional interface, and without it every rename, the kernel's silly-rename
// included, costs the opener an ESTALE. Since rootHandler always implements
// mover, go-nfs never falls back to invalidating, so that fallback is here.
func (h *rootHandler) Rename(sourceFS billy.Filesystem, source []string, destFS billy.Filesystem, dest []string) error {
	if m, ok := h.Handler.(mover); ok {
		return m.Rename(sourceFS, source, destFS, dest)
	}
	return h.InvalidateHandle(sourceFS, h.ToHandle(sourceFS, source))
}

// VerifierFor and DataForVerifier are READDIR cookie business, which go-nfs
// asks for through a separate optional interface (nfs.CachingHandler). They are
// forwarded explicitly because embedding nfs.Handler does not carry them, and
// losing them changes directory listing without failing anything.
func (h *rootHandler) VerifierFor(path string, contents []fs.FileInfo) uint64 {
	if c, ok := h.Handler.(nfs.CachingHandler); ok {
		return c.VerifierFor(path, contents)
	}
	return 0
}

func (h *rootHandler) DataForVerifier(path string, id uint64) []fs.FileInfo {
	if c, ok := h.Handler.(nfs.CachingHandler); ok {
		return c.DataForVerifier(path, id)
	}
	return nil
}
