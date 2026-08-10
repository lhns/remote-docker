// Package nfsserve is the client's in-process NFSv3 server.
//
// It replaces `rclone serve nfs`, which the previous clients downloaded at
// runtime. Owning the server buys two things rclone could not give us: an
// export namespace we control, so bind sources anywhere on this machine can be
// served through one listener and one tunnel port (ADR 0007), and synthesised
// ownership, so files do not all appear as uid 1000 with chown failing
// (ADR 0004).
package nfsserve

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/memfs"
	nfs "github.com/willscott/go-nfs"
	"github.com/willscott/go-nfs/helpers"
)

// handleCacheSize bounds the file-handle cache.
//
// NFSv3 handles are opaque and the client may present one at any later point,
// so an entry evicted while still in use surfaces as ESTALE. A source tree has
// a lot of files; this is deliberately generous, matching the
// --nfs-cache-handle-limit the shell clients passed to rclone.
const handleCacheSize = 1_000_000

// Server exports a Registry over NFSv3.
type Server struct {
	registry *Registry
	handler  nfs.Handler
}

// New builds a server over the given registry.
func New(registry *Registry) *Server {
	h := &mountHandler{registry: registry}
	return &Server{
		registry: registry,
		// The caching handler supplies ToHandle/FromHandle and the directory
		// verifiers; implementing those correctly is not our business.
		handler: helpers.NewCachingHandler(h, handleCacheSize),
	}
}

// Serve accepts connections until l is closed.
//
// l is normally a listener obtained from the SSH connection, so the server is
// reachable at 127.0.0.1:<port> inside the workspace and nowhere else. It is
// never bound on a real interface.
func (s *Server) Serve(l net.Listener) error {
	err := nfs.Serve(l, s.handler)
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// mountHandler resolves a MOUNT request to the share it names.
//
// This is where the virtual namespace lives. go-nfs hands us the requested
// directory and lets us return the filesystem for it, so each mount gets a
// filesystem bound to its own share -- no mux filesystem, and no path in one
// share can address another.
type mountHandler struct {
	registry *Registry
}

// refusedFS is returned alongside every failing mount status.
//
// go-nfs v0.0.4's onMount calls Handler.ToHandle on the returned filesystem
// before checking the status, and the caching handler dereferences it, so
// returning nil, the obvious thing for a refusal, panics the server. Any
// client could then crash this process just by asking for a path that does not
// exist, which is precisely the boundary we rely on refusing. An empty
// in-memory filesystem satisfies ToHandle; the handle is discarded unhandled
// because the status is not Ok.
var refusedFS = memfs.New()

func (h *mountHandler) Mount(_ context.Context, _ net.Conn, req nfs.MountRequest) (nfs.MountStatus, billy.Filesystem, []nfs.AuthFlavor) {
	auths := []nfs.AuthFlavor{nfs.AuthFlavorNull}

	share, rest, ok := h.registry.Lookup(string(req.Dirpath))
	if !ok {
		// Not an error worth logging loudly: an unregistered path is the
		// normal answer for a stale mount attempt after a share was dropped.
		return nfs.MountStatusErrNoEnt, refusedFS, auths
	}

	if rest == "/" || rest == "" {
		return nfs.MountStatusOk, share.fs, auths
	}

	// Mounting a subdirectory of a share. Chroot keeps the bound-OS boundary,
	// so this cannot be used to climb out.
	sub, err := share.fs.Chroot(rest)
	if err != nil {
		return nfs.MountStatusErrNoEnt, refusedFS, auths
	}
	return nfs.MountStatusOk, sub, auths
}

// Change enables SETATTR. Returning nil here would make go-nfs treat the
// export as read-only.
func (h *mountHandler) Change(fs billy.Filesystem) billy.Change {
	inner, _ := fs.(billy.Change)
	return &attrChange{inner: inner}
}

// FSStat reports free space. Docker and ordinary tools query it; zeroes would
// make a build tool believe the disk is full.
func (h *mountHandler) FSStat(_ context.Context, _ billy.Filesystem, stat *nfs.FSStat) error {
	// The real figure belongs to whichever local volume backs the share, and
	// obtaining it portably is more trouble than it is worth. Report a large
	// but finite size: writes fail with the underlying error if the disk is
	// genuinely full, which is a clearer signal than a client that refused to
	// try.
	const tb = uint64(1) << 40
	stat.TotalSize = tb
	stat.FreeSize = tb
	stat.AvailableSize = tb
	stat.TotalFiles = 1 << 40
	stat.FreeFiles = 1 << 40
	stat.AvailableFiles = 1 << 40
	stat.CacheHint = 0
	return nil
}

// The caching handler supplies these; they are unreachable in practice.
func (h *mountHandler) ToHandle(billy.Filesystem, []string) []byte {
	panic("nfsserve: ToHandle must be provided by the caching handler")
}

func (h *mountHandler) FromHandle([]byte) (billy.Filesystem, []string, error) {
	return nil, nil, fmt.Errorf("nfsserve: FromHandle must be provided by the caching handler")
}

func (h *mountHandler) InvalidateHandle(billy.Filesystem, []byte) error { return nil }

func (h *mountHandler) HandleLimit() int { return handleCacheSize }
