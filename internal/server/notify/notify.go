// Package notify replays the client's filesystem changes inside the workspace,
// so that a watcher in a container sees them.
//
// NFS carries no change notification, so a container watching a bind-mounted
// directory sees nothing at all when the user edits a file on their own
// machine (ADR 0014). Linux offers no way to inject a synthetic event --
// fanotify(7) says so outright -- so the only mechanism, here or anywhere, is
// to perform a real VFS operation and let the kernel emit the event as a side
// effect. ADR 0016 records which operations produce which events, measured.
package notify

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lhns/remote-docker/pkg/workspace"
)

// Logger matches the convention used elsewhere in the agent.
type Logger interface {
	Printf(format string, args ...any)
}

// Volumes resolves a managed volume to the directory dockerd has it mounted
// at.
//
// That directory is the whole trick. dockerd's local driver mounts each NFS
// volume once and bind-mounts it into every container using it, and a bind
// mount shares the superblock -- so the inode there is the same inode a
// watcher inside the container put its mark on. Measured, not assumed: see
// ADR 0016. It means no container enumeration, no PID lookup and no entering
// anyone's mount namespace.
type Volumes interface {
	Mountpoint(ctx context.Context, volume string) (string, error)
}

// Poker performs the operation that makes a watcher notice a path.
//
// An interface so the protocol above it -- framing, validation, path
// resolution, coarsening -- is testable on a machine with no Linux kernel,
// which is every development machine this project has.
type Poker interface {
	Poke(path string, isDir bool) error
}

// mountpointTTL is how long a resolved volume mountpoint is trusted. Short,
// because a volume is unmounted when its last container stops and the path
// then refers to an empty directory on the workspace's own disk rather than to
// the share.
const mountpointTTL = 10 * time.Second

// Replayer serves one client's change stream.
type Replayer struct {
	Volumes Volumes
	Poker   Poker
	Log     Logger

	// Extra maps an export to an additional directory to poke, for mounts the
	// agent made itself rather than dockerd -- the interactive shell's
	// ~/workspace, which is a second mount of the same export.
	Extra map[string]string

	mu     sync.Mutex
	cached map[string]cachedMountpoint
}

type cachedMountpoint struct {
	path string
	at   time.Time
}

// Serve reads frames until the stream ends.
//
// The hello line goes first and goes unconditionally. An agent too old to know
// this command falls through to the generic exec path and runs
// `sh -c "workspace-notify"`, which exits 127 -- so the client has to be able
// to tell a working channel from a missing one, and a greeting is the only
// thing that distinguishes them.
func (r *Replayer) Serve(ctx context.Context, rw io.ReadWriter) error {
	hello, err := json.Marshal(workspace.NotifyFrame{
		Hello: &workspace.NotifyHello{Version: workspace.NotifyVersion},
	})
	if err != nil {
		return err
	}
	if _, err := rw.Write(append(hello, '\n')); err != nil {
		return fmt.Errorf("notify: greeting the client: %w", err)
	}

	scanner := bufio.NewScanner(rw)
	scanner.Buffer(make([]byte, 0, 64*1024), workspace.MaxNotifyFrame)

	for scanner.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var frame workspace.NotifyFrame
		if err := json.Unmarshal(line, &frame); err != nil {
			// A malformed frame is a bug on the client, not a reason to tear
			// down a working session: the next frame is very likely fine.
			r.logf("notify: ignoring a malformed frame: %v", err)
			continue
		}
		r.apply(ctx, frame)
	}
	return scanner.Err()
}

// apply replays one frame.
func (r *Replayer) apply(ctx context.Context, frame workspace.NotifyFrame) {
	if n := frame.Notice; n != nil {
		// The client is telling us its own picture is incomplete. The best
		// available answer is one coarse poke at the directory covering what
		// was lost: a watcher that rescans notices, and replaying events we
		// never received is not on offer.
		r.logf("notify: the client dropped %d changes under %s%s (%s); poking the directory instead",
			n.Dropped, n.Export, n.Path, n.Reason)
		r.poke(ctx, n.Export, cleanShare(n.Path), true)
		return
	}

	// Directories are collected and poked once per frame, per export. A save
	// touching five hundred files in one directory must not become five
	// hundred identical syscalls.
	dirs := map[string]map[string]bool{}
	note := func(export, dir string) {
		if dirs[export] == nil {
			dirs[export] = map[string]bool{}
		}
		dirs[export][dir] = true
	}

	for _, e := range frame.Events {
		// Re-validated here even though the client validates before sending.
		// This stream tells a root process which path to touch, and neither
		// end may assume the other checked.
		if err := e.Validate(); err != nil {
			r.logf("notify: refusing an event: %v", err)
			continue
		}

		switch {
		case e.Op&(workspace.OpRemove|workspace.OpRename) != 0:
			// Nothing to touch: the file is gone, and unlink of a name that is
			// already gone fails with ENOENT before the kernel generates
			// anything (measured; ADR 0014 stays open on exactly this). The
			// parent directory is all that can be said.
			note(e.Export, parentDir(e.Path))

		case e.Op&workspace.OpCreate != 0:
			// The file itself, so a watcher keyed on the file sees it, and the
			// parent, so one keyed on the directory rescans and finds it.
			//
			// Deliberately NOT open(O_CREAT), even though that was measured to
			// produce a real IN_CREATE. Between the client observing the
			// creation and this replay the file may have been deleted again,
			// and O_CREAT would then create it -- writing to the user's own
			// filesystem, through the very export we are notifying about.
			// Replay must never mutate; IN_CREATE is worth less than that.
			r.poke(ctx, e.Export, e.Path, e.Dir)
			note(e.Export, parentDir(e.Path))

		default:
			r.poke(ctx, e.Export, e.Path, e.Dir)
		}
	}

	for export, set := range dirs {
		for dir := range set {
			r.poke(ctx, export, dir, true)
		}
	}
}

// poke resolves an in-share path and performs the operation.
func (r *Replayer) poke(ctx context.Context, export, share string, isDir bool) {
	for _, root := range r.roots(ctx, export) {
		abs, ok := resolve(root, share)
		if !ok {
			r.logf("notify: %s%s does not resolve under %s", export, share, root)
			continue
		}
		if err := r.Poker.Poke(abs, isDir); err != nil {
			// A path that is not there is the ordinary case, not a fault: the
			// container may not have looked at it, or it may have changed
			// again since. Logging every one would drown the log during a
			// build.
			r.debugf("notify: poking %s: %v", abs, err)
		}
	}
}

// roots is every directory in the workspace holding this export: the volume
// dockerd mounted, plus any mount the agent made itself.
//
// Both are poked because they are separate mounts of the same export and,
// unlike a bind mount, separate mounts do not necessarily share an inode.
func (r *Replayer) roots(ctx context.Context, export string) []string {
	var out []string
	if mp, err := r.mountpoint(ctx, export); err == nil && mp != "" {
		out = append(out, mp)
	}
	if extra, ok := r.Extra[export]; ok && extra != "" {
		out = append(out, extra)
	}
	return out
}

func (r *Replayer) mountpoint(ctx context.Context, export string) (string, error) {
	volume, err := workspace.VolumeNameForExport(export)
	if err != nil {
		return "", err
	}

	r.mu.Lock()
	if c, ok := r.cached[volume]; ok && time.Since(c.at) < mountpointTTL {
		r.mu.Unlock()
		return c.path, nil
	}
	r.mu.Unlock()

	mp, err := r.Volumes.Mountpoint(ctx, volume)
	if err != nil {
		// A share no container has mounted yet is ordinary: the user shared a
		// directory and has not started anything using it. Cached as empty so
		// a stream of events does not become a stream of docker calls.
		mp = ""
	}

	r.mu.Lock()
	if r.cached == nil {
		r.cached = map[string]cachedMountpoint{}
	}
	r.cached[volume] = cachedMountpoint{path: mp, at: time.Now()}
	r.mu.Unlock()

	if mp == "" {
		return "", err
	}
	return mp, nil
}

// resolve joins an in-share path onto a root and confirms the result is still
// under it.
//
// workspace.FSEvent.Validate has already refused "..", so this cannot fail for
// a validated event. It is here because the consequence of being wrong is a
// root process touching an arbitrary path, and a check that can only ever be
// redundant is the right price for that.
func resolve(root, share string) (string, bool) {
	rel := strings.TrimPrefix(path.Clean("/"+share), "/")
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if abs != root && !strings.HasPrefix(abs, root+string(filepath.Separator)) {
		return "", false
	}
	return abs, true
}

// parentDir is the in-share directory holding a path.
func parentDir(p string) string {
	d := path.Dir(cleanShare(p))
	if d == "." {
		return "/"
	}
	return d
}

// cleanShare normalises an in-share path without letting it escape: a notice
// names a directory, and its path comes off the wire like any other.
func cleanShare(p string) string {
	return path.Clean("/" + strings.TrimPrefix(p, "/"))
}

func (r *Replayer) logf(format string, args ...any) {
	if r.Log != nil {
		r.Log.Printf(format, args...)
	}
}

// debugf is for the ordinary, expected failures -- a path that has changed
// again, a container that never looked at it. Kept separate so raising the
// log level is a choice rather than a flood.
func (r *Replayer) debugf(string, ...any) {}
