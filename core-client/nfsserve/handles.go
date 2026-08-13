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

	"github.com/go-git/go-billy/v5"
	nfs "github.com/willscott/go-nfs"
)

// Handle tags. A byte rather than a length check: both kinds would otherwise
// be distinguished by how many bytes go-nfs's uuid happens to occupy, which is
// nothing this package controls.
const (
	tagRoot   = 0x01
	tagCached = 0x02
)

// exportKeySize is how much of the export path's digest a root handle carries.
// A handle may be 64 bytes (RFC 1813); a root spends 9 plus the cache's own.
const exportKeySize = 8

// rootHandler answers for share roots and delegates everything else.
type rootHandler struct {
	nfs.Handler // the caching handler: verifiers, and every handle but a root

	registry *Registry
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
	if len(path) != 0 || export == "" {
		return append([]byte{tagCached}, cached...)
	}

	// The cache's answer FIRST and the derived key behind it. While this
	// process lives, every root operation is resolved exactly as it was before
	// any of this existed; the key is what answers once the cache is gone,
	// which is the only case this feature is for. Making the derived key the
	// primary answer changed behaviour in the live process too, and the suite
	// found it: every mount worked and every read said "permission denied".
	out := append([]byte{tagRoot}, exportKey(export)...)
	return append(out, cached...)
}

func (h *rootHandler) FromHandle(handle []byte) (billy.Filesystem, []string, error) {
	if len(handle) == 0 {
		return nil, nil, errStaleExport
	}
	switch handle[0] {
	case tagRoot:
		if len(handle) < 1+exportKeySize {
			return nil, nil, errStaleExport
		}
		key, cached := handle[1:1+exportKeySize], handle[1+exportKeySize:]

		// The cache, exactly as before, for as long as this process holds it.
		if len(cached) > 0 {
			if fs, path, err := h.Handler.FromHandle(cached); err == nil {
				return fs, path, nil
			}
		}

		// And the derived key when it cannot answer, which is what a client
		// that has restarted is asking with.
		share, ok := h.shareForKey(key)
		if !ok {
			return nil, nil, errStaleExport
		}
		return share.fs, []string{}, nil
	case tagCached:
		return h.Handler.FromHandle(handle[1:])
	default:
		// A handle from a build that tagged nothing. Stale is the honest
		// answer and the kernel recovers by looking up again.
		return nil, nil, errStaleExport
	}
}

func (h *rootHandler) InvalidateHandle(f billy.Filesystem, handle []byte) error {
	// A root handle is derived rather than stored, so there is nothing to
	// forget; invalidating it would only make the mount unusable.
	if len(handle) > 0 && handle[0] == tagCached {
		return h.Handler.InvalidateHandle(f, handle[1:])
	}
	return nil
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
