// Package unions owns the union mounts behind delegated shares (ADR 0044).
//
// One per share per account: the client's export underneath, a cache volume on
// top, and the merged view a container binds. This package is the glue --
// resolving an account to its daemon, asking that daemon where a volume's data
// lives, and supervising the process that serves the union. The mechanics of
// entering a namespace and mounting are core-agent/union, which knows nothing
// about Docker.
package unions

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lhns/remote-docker/core-agent/netns"
	"github.com/lhns/remote-docker/core-agent/union"
	"github.com/lhns/remote-docker/core/cache"
	"github.com/lhns/remote-docker/core/logx"
	"github.com/lhns/remote-docker/core/workspace"
)

// restartDelay is how long a failed union waits before being mounted again.
//
// The same two seconds supervise.Dockerd uses, and for the same reason: long
// enough not to spin on a permanent failure, short enough that a transient one
// is over before anybody looks.
const restartDelay = 2 * time.Second

// aliveTimeout bounds the liveness check. A union whose server is gone answers
// ENOTCONN immediately; one whose server is wedged answers nothing at all, and
// waiting on it would make every request that asks wait too.
const aliveTimeout = 2 * time.Second

// stopTimeout bounds waiting for a stopped union's supervisor to finish, so one
// wedged child cannot hold up every other share.
const stopTimeout = 2 * time.Second

// readyTimeout is how long Prepare waits for a new union to answer.
//
// Generous because the lower is an NFS mount made over the CLIENT's link, and
// what is being waited for is a round trip to the other side of the world
// rather than a local mount. A container refused here is a container that does
// not start, so the cost of being impatient is higher than the cost of waiting.
const readyTimeout = 90 * time.Second

// Volumes tells the manager where a volume's data lives, as the daemon sees it.
type Volumes interface {
	// RawMountpoint is the path INSIDE the daemon's own filesystem, not
	// relocated through /proc/<pid>/root. The union is mounted in that
	// daemon's namespace, so a relocated path would name nothing there.
	RawMountpoint(ctx context.Context, host, volume string) (string, error)

	// MountSources is every host path a running container has bound, which is
	// how a union still in use is recognised.
	MountSources(ctx context.Context, host string) (map[string]bool, error)
}

// Daemon is what the manager needs to know about the daemon serving an account.
type Daemon struct {
	// Host is the daemon as a DOCKER_HOST value, empty for the shared one.
	Host string

	// PID is the process whose mount namespace the union belongs in. Zero is
	// the agent's own, which is the shared-daemon mode.
	PID int
}

// Manager mounts, supervises and releases unions.
type Manager struct {
	// Self is how the agent runs itself again, since the union is served by a
	// child that re-enters a namespace before becoming fuse-overlayfs.
	Self string

	Volumes Volumes
	Log     *slog.Logger

	mu     sync.Mutex
	shares map[string]*live
}

// live is one mounted union.
type live struct {
	spec   union.Spec
	host   string // the daemon this union is mounted in, for asking who holds it
	cache  string // the managed volume holding the layer, for the collector
	cancel context.CancelFunc
	done   chan struct{}

	// applied is what the client's fill and invalidations wrote through this
	// union, by path.
	//
	// The cache is written through the merged mount (ADR 0044), so the filled
	// copy of every file is in the cache LAYER, which is what Changes reads --
	// and without this an idle session is told about the whole tree every few
	// seconds forever. The client checks the same thing against its own
	// manifest, and that check is the rule; this one keeps the answer
	// proportional to what actually changed.
	//
	// Lost on an agent restart, which costs one oversized reply and no
	// correctness: the client's manifest still decides.
	appliedMu sync.Mutex
	applied   map[string]applied
}

// applied is one file as the client's stream left it.
type applied struct {
	size    int64
	modTime time.Time
}

// noteApplied records what was just written through the union.
func (l *live) noteApplied(name string, size int64, modTime time.Time) {
	l.appliedMu.Lock()
	defer l.appliedMu.Unlock()
	if l.applied == nil {
		l.applied = map[string]applied{}
	}
	l.applied[name] = applied{size: size, modTime: modTime}
}

// forgetApplied drops a path, so a file the client removed and the container
// later recreates is reported.
func (l *live) forgetApplied(name string) {
	l.appliedMu.Lock()
	defer l.appliedMu.Unlock()
	delete(l.applied, name)
}

// isApplied reports whether an entry is exactly what the client last wrote.
func (l *live) isApplied(name string, size int64, modTime time.Time) bool {
	l.appliedMu.Lock()
	defer l.appliedMu.Unlock()
	a, ok := l.applied[name]
	return ok && a.size == size && a.modTime.Equal(modTime)
}

// ErrNoShare is what every op answers for a share this workspace is not
// serving. Named so the client can stop asking rather than guess from a string.
var ErrNoShare = errors.New("no union for this share")

// key names a share within an account, since two accounts may share a name for
// the same directory and must never share a mount.
func key(account, export string) string { return account + "\x00" + export }

// ownedBy reports whether a share key belongs to an account, and exists so
// that only key() above knows how one is put together.
func ownedBy(k, account string) bool { return strings.HasPrefix(k, account+"\x00") }

// Prepare mounts a share's union if it is not already mounted, and answers with
// the path a container binds.
//
// Idempotent, because preparing twice is ordinary: a client reconnects, or a
// second container wants the same directory. A share already mounted and alive
// answers with the same path and does nothing else.
func (m *Manager) Prepare(ctx context.Context, account, client string, d Daemon, req cache.Request) (string, error) {
	// DERIVED, not taken from the request: Validate only asks whether the name
	// is a managed one, which every machine of an account satisfies for every
	// other machine's (ADR 0029). See workspace.CacheVolumeForExport.
	expected, err := workspace.CacheVolumeForExport(client, req.Export)
	if err != nil {
		return "", fmt.Errorf("unions: %w", err)
	}
	if req.Cache != expected {
		return "", fmt.Errorf("unions: %s asked to cache %s in %q, which is not this machine's cache volume",
			account, req.Export, req.Cache)
	}

	if err := req.Validate(); err != nil {
		return "", err
	}

	cacheDir, err := m.Volumes.RawMountpoint(ctx, d.Host, req.Cache)
	if err != nil {
		return "", fmt.Errorf("unions: finding the cache volume for %s: %w", req.Export, err)
	}

	// Empty is an older client: ReadCached (core/cache.Request.Read).
	read := workspace.Read(req.Read)
	if read == workspace.ReadUnset {
		read = workspace.ReadCached
	}

	spec := union.Spec{
		PID:      d.PID,
		Export:   req.Export,
		Port:     req.Port,
		Read:     read,
		CacheDir: cacheDir,
	}
	if err := spec.Validate(); err != nil {
		return "", err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shares == nil {
		m.shares = map[string]*live{}
	}

	k := key(account, req.Export)
	if existing, ok := m.shares[k]; ok {
		if m.alive(existing.spec) {
			return existing.spec.Merged(), nil
		}
		// Down rather than absent. Torn out here so the mount below is a fresh
		// one; a container already bound to the dead mount is not repaired by
		// this and cannot be, which is the rule CLAUDE.md states about a mount
		// that has gone wrong.
		m.stop(k, existing)
	}

	l := m.start(spec, d.Host)
	l.cache = req.Cache
	m.shares[k] = l

	if err := m.waitReady(ctx, spec); err != nil {
		m.stop(k, l)
		return "", err
	}
	return spec.Merged(), nil
}

// start runs the union's server and keeps running it.
func (m *Manager) start(spec union.Spec, host string) *live {
	ctx, cancel := context.WithCancel(context.Background())
	l := &live{spec: spec, host: host, cancel: cancel, done: make(chan struct{})}

	go func() {
		defer close(l.done)
		for ctx.Err() == nil {
			// Adopted rather than replaced. After an agent restart the child
			// is an orphan whose mount is still serving every container bound
			// to it, and mounting over that would strand them: a container
			// keeps the mount it already has, and a second one stacked on the
			// path cannot repair it. So the supervisor waits for a serving
			// mount to go before it makes another.
			//
			// Safe only because "alive" means MOUNTED rather than "the path is
			// there" (ADR 0044). Against a stat this would wait forever on the
			// empty directory a dead union leaves behind, and the share would
			// never come back.
			if m.awaitGone(ctx, spec) {
				continue
			}

			err := m.run(ctx, spec)
			if ctx.Err() != nil {
				return
			}
			logx.Or(m.Log).Warn("a union exited; mounting it again",
				"export", spec.Export, "err", err, "in", restartDelay)
			select {
			case <-ctx.Done():
				return
			case <-time.After(restartDelay):
			}
		}
	}()
	return l
}

// awaitGone waits while a mount this process did not start is still serving,
// and reports whether it waited at all.
//
// The share works throughout: what is being waited for is the right to mount
// again, not the mount itself.
func (m *Manager) awaitGone(ctx context.Context, spec union.Spec) bool {
	if !m.alive(spec) {
		return false
	}
	logx.Or(m.Log).Info("adopting a union that is already mounted",
		"export", spec.Export, "merged", spec.Merged())

	for m.alive(spec) {
		select {
		case <-ctx.Done():
			return true
		case <-time.After(restartDelay):
		}
	}
	logx.Or(m.Log).Info("the adopted union went; mounting one of ours",
		"export", spec.Export)
	return true
}

// run is one attempt: the agent re-executed as the child that enters the
// daemon's namespace and becomes fuse-overlayfs.
func (m *Manager) run(ctx context.Context, spec union.Spec) error {
	cmd := union.Reexec(ctx, m.Self, union.ModeServe, spec)
	// Both to stderr, as supervise.Dockerd does: what fuse-overlayfs says is
	// the only account of why a union failed, and losing it would leave a
	// share that does not work and says nothing.
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("unions: starting the union for %s: %w", spec.Export, err)
	}
	return cmd.Wait()
}

// waitReady blocks until the union answers, or gives up.
//
// The mount is what is waited for, not the process: fuse-overlayfs is up before
// its mount is, and a container bound to a mountpoint that is not yet a mount
// sees an empty directory rather than an error.
func (m *Manager) waitReady(ctx context.Context, spec union.Spec) error {
	started := time.Now()
	deadline := started.Add(readyTimeout)
	for {
		if m.alive(spec) {
			return nil
		}
		if time.Now().After(deadline) {
			// How long, because the budget is the likeliest thing to be wrong:
			// the lower is an NFS mount over the client's link, and a slow one
			// takes longer to mount than a fast one. Without the number the
			// message reads as a union that cannot work rather than one that
			// was not given time.
			return fmt.Errorf("unions: the union for %s did not come up in %s",
				spec.Export, time.Since(started).Round(time.Second))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// alive reports whether the union answers. The mount is the truth, not the pid:
// see union.Alive.
func (m *Manager) alive(spec union.Spec) bool {
	ctx, cancel := context.WithTimeout(context.Background(), aliveTimeout)
	defer cancel()
	return union.Alive(ctx, spec) == nil
}

// ReleaseAccount drops the shares an account holds that nothing is using,
// which is what a cache session ending means.
//
// NOT every share: the union outlives the channel that asked for it, because
// the channel goes whenever the connection under it is released (ADR 0015)
// while the containers bound to the union keep running (ADR 0044).
func (m *Manager) ReleaseAccount(ctx context.Context, account string) {
	held := m.heldByContainers(ctx, account)

	m.mu.Lock()
	var specs []union.Spec
	for k, l := range m.shares {
		if !ownedBy(k, account) {
			continue
		}
		if held[l.spec.Merged()] {
			// A container is still bound to it. Unmounting now frees nothing --
			// the container keeps the mount it already has, and keeps it BROKEN,
			// because a mount that has gone wrong stays wrong until the last
			// container lets go of it. Released instead the next time the account
			// disconnects with nothing holding this share.
			continue
		}
		specs = append(specs, l.spec)
		m.stop(k, l)
	}
	m.mu.Unlock()

	for _, spec := range specs {
		if err := union.Unmount(ctx, m.Self, spec); err != nil {
			logx.Or(m.Log).Warn("could not unmount a union", "export", spec.Export, "err", err)
		}
	}
}

// heldByContainers is the set of this account's union mounts that a running
// container is still bound to.
//
// A union is bound into a container by PATH rather than as a volume, so nothing
// else in the workspace knows the two are related and the daemon is the only
// thing that can say. On any doubt this answers "held": keeping a mount nobody
// needs costs a process, while taking one that is in use costs somebody's
// container permanently, since a mount that has gone wrong stays wrong until
// the last container lets go of it.
func (m *Manager) heldByContainers(ctx context.Context, account string) map[string]bool {
	m.mu.Lock()
	hosts := map[string]bool{}
	shares := map[string]bool{}
	for k, l := range m.shares {
		if ownedBy(k, account) {
			hosts[l.host] = true
			shares[l.spec.Merged()] = true
		}
	}
	m.mu.Unlock()

	held := map[string]bool{}
	for host := range hosts {
		sources, err := m.Volumes.MountSources(ctx, host)
		if err != nil {
			logx.Or(m.Log).Warn("cannot tell which unions are in use; keeping them",
				"account", account, "err", err)
			return shares
		}
		for source := range sources {
			if shares[source] {
				held[source] = true
			}
		}
	}
	return held
}

// MountedCaches names the cache volumes that have a union on them, for the
// machine asking.
//
// What the client's volume collector needs and cannot work out: no container
// references a cache volume, because a union is bound by path, so the daemon
// reports it unused and the collector empties it under a running container
// (ADR 0044).
//
// Answered from the FILESYSTEM as well as from this process's own record, and
// the filesystem is the half that matters. A union outlives the agent that
// started it, so after a restart the mounts are serving and this manager knows
// nothing about them -- and a truthful "none mounted" then costs somebody the
// contents of a cache their container is still reading. The share ids come from
// the mounts; the client digest comes from the key that authenticated, so the
// names are this machine's own and no other machine's are named.
func (m *Manager) MountedCaches(account, client string, d Daemon) []string {
	names := map[string]bool{}

	m.mu.Lock()
	for k, l := range m.shares {
		if ownedBy(k, account) && l.cache != "" {
			names[l.cache] = true
		}
	}
	m.mu.Unlock()

	if client != "" {
		for _, id := range union.MountedShares(netns.Root(d.PID)) {
			names[workspace.VolumeNameForCache(client, id)] = true
		}
	}

	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// stop ends supervision. The caller holds the lock.
func (m *Manager) stop(k string, l *live) {
	l.cancel()
	delete(m.shares, k)
	select {
	case <-l.done:
	case <-time.After(stopTimeout):
	}
}

// Capability reports whether this workspace can serve a union at all, as
// workspace.Info spells it.
//
// Asked of the daemon that would serve it, because that is where the answer
// differs: in per-account mode the binary has to be in the image that daemon
// runs (agent/internal/daemons/plan.go:38), and the agent's own filesystem
// says nothing about it.
func Capability(root string) string {
	if root == "" {
		root = "/"
	}
	if _, err := os.Stat(path.Join(root, "dev/fuse")); err != nil {
		return workspace.UnionNoDevice
	}
	for _, dir := range []string{"usr/bin", "bin", "usr/local/bin", "usr/sbin", "sbin"} {
		if _, err := os.Stat(path.Join(root, dir, union.Binary)); err == nil {
			return workspace.UnionReady
		}
	}
	return workspace.UnionNoBinary
}
