// Package nfsserve is the client's in-process NFSv3 server: one export
// namespace for bind sources anywhere on this machine (ADR 0007), with
// synthesised ownership (ADR 0004).
package nfsserve

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/memfs"
	"github.com/lhns/remote-docker/core/logx"

	nfs "github.com/willscott/go-nfs"
	"github.com/willscott/go-nfs/helpers"
)

// handleCacheSize bounds the file-handle cache. Generous, because a handle
// evicted while a client still holds it surfaces as ESTALE.
const handleCacheSize = 1_000_000

// Server exports a Registry over NFSv3.
type Server struct {
	registry *Registry
	handler  nfs.Handler
}

// New builds a server over the given registry. A nil logger is silence.
func New(registry *Registry, log *slog.Logger) *Server {
	return newServer(registry, log, handleCacheSize)
}

// newServer is New with the handle cache limit as a parameter, so a test can
// ask what happens when the cache fills without minting a million handles.
func newServer(registry *Registry, log *slog.Logger, limit int) *Server {
	h := &mountHandler{registry: registry, limit: limit}
	return &Server{
		registry: registry,
		// The caching handler supplies the directory verifiers and every handle
		// but a share root, which rootHandler derives (ADR 0033).
		handler: &rootHandler{
			Handler:  helpers.NewCachingHandler(h, limit),
			registry: registry,
			log:      logx.Or(log),
		},
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
// filesystem bound to its own share. No mux filesystem, and no path in one
// share can address another.
type mountHandler struct {
	registry *Registry
	limit    int
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

	share, rest, ok := h.registry.LookupOrRestore(string(req.Dirpath))
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
// export as read-only, and asserting the filesystem to billy.Change returns
// exactly that: osfs implements no such interface. The failure is silent, a
// chmod reported as done and a built binary that cannot be run.
//
// Root() is the share's directory, which the names reaching attrChange are
// relative to. For a single-file share that directory is the one CONTAINING
// the file, so the change goes through singleFileChange, which refuses every
// name but the file itself.
func (h *mountHandler) Change(fs billy.Filesystem) billy.Change {
	c := &attrChange{root: fs.Root()}
	if one, ok := singleFileOf(fs); ok {
		return &singleFileChange{attrChange: c, fs: one}
	}
	return c
}

// FSStat reports a large, finite free space rather than the real figure, which
// has no portable source. Zeroes would make a build tool believe the disk is
// full; a disk that really is full fails the write with its own error.
func (h *mountHandler) FSStat(_ context.Context, _ billy.Filesystem, stat *nfs.FSStat) error {
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

func (h *mountHandler) HandleLimit() int { return h.limit }
