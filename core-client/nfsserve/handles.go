package nfsserve

// A share's root handle, which has to outlive this process.
//
// NFSv3 handles are opaque and the kernel keeps presenting the ones it holds,
// because the protocol promises they stay valid for the life of the file --
// across server restarts. go-nfs mints a uuid per path into an in-memory cache,
// so a restarted client cannot resolve any of them, and every container that
// was running reads "Stale file handle" against a mount that still looks fine.
//
// One handle matters more than the others. MOUNT returns the handle for the
// share root (go-nfs mount.go:42), and the kernel NEVER mounts again: once that
// one stops resolving, every lookup starts from something dead and nothing can
// recover. Below the root, Linux retries a path lookup once on ESTALE with
// LOOKUP_REVAL, so given a root that answers it can ask again for everything
// underneath and get fresh handles as it goes.
//
// So the root is derived from the export path and the rest stay a cache. See
// ADR 0033, which also records why per-file handles were measured and rejected.

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
// Told apart by LENGTH, which is not the tidy way round. Prefixing every handle
// with a tag byte instead -- the obvious way to carry two formats -- makes
// every mount succeed and every read fail with "permission denied", with the
// workspace daemon unable to open the volume's own directory. Nothing in go-nfs
// or here explains why an ordinary handle must keep the size it was given;
// what is established is the measurement, and it is enough to rule the tidier
// version out.
//
// Recognising by length means depending on those handles being a fixed 16
// bytes, so a test pins it rather than trusting it.
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

// errStaleExport is what a handle naming a share that is no longer exported
// gets. Never a lookup, never a guess: a handle may name a capability the
// workspace still holds, and may not resurrect one (ADR 0027).
var errStaleExport = errors.New("nfsserve: no such export")

func (h *rootHandler) ToHandle(f billy.Filesystem, path []string) []byte {
	cached := h.Handler.ToHandle(f, path)

	// Only the share's own root, never a Chroot into a subdirectory of it:
	// that mount resolves against the subdirectory, and giving it the share's
	// handle would serve the wrong directory to a client that asked correctly.
	export := exportRootOf(f)
	if len(path) != 0 || export == "" || len(cached) != cachedHandleSize {
		return cached
	}

	// The cache's answer FIRST and the derived key behind it. While this
	// process lives, every root operation resolves exactly as it did before any
	// of this existed; the key answers once the cache is gone, which is the
	// only case this feature is for.
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

	// The cache, exactly as before, for as long as this process holds it.
	if fs, path, err := h.Handler.FromHandle(cached); err == nil {
		return fs, path, nil
	}

	// And the derived key when it cannot answer, which is what a client that
	// has restarted is asking with.
	share, ok := h.shareForKey(key)
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

// shareForKey finds the share whose export path matches a root handle.
//
// Asked of the registry every time rather than cached, so a handle can only
// ever reach a share exported RIGHT NOW. There are a handful of shares -- one
// per bind mount -- so this is a walk of three or four entries.
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
