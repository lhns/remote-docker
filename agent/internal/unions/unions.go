// Package unions owns the union mounts behind delegated shares (ADR 0044).
//
// One per share per machine: the client's export underneath, a cache volume on
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
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lhns/remote-docker/core-agent/netns"
	"github.com/lhns/remote-docker/core-agent/replay"
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

	mu sync.Mutex

	shares map[string]*live

	// pending is the share keys a Prepare is mounting, so a second Prepare for
	// the same share waits rather than stacking a second fuse-overlayfs on one
	// upper (ADR 0044). Single-flighted rather than locked, in the shape of
	// daemons.Manager.ensure: mounting takes up to readyTimeout, and every
	// other cache operation reaches m.share, so under m.mu they would all
	// queue behind one cold union.
	pending map[string]chan struct{}

	// sessions counts each machine's open cache sessions, by owner(); a
	// machine with none has no entry. prepares numbers every successful
	// Prepare, which stamps the union it answered with (live.prepared), so a
	// Release can tell a union prepared while it asked the daemon (Open).
	sessions map[string]int
	prepares uint64

	// probe answers whether a union is serving, and onStart is told about each
	// one this manager mounts. Nil in production, where they are union.Alive
	// and nothing. Tests need both: no union can be mounted on a development
	// machine, and a second one started for a share replaces the first in
	// m.shares, so nothing else would show that it happened.
	probe   func(context.Context, union.Spec) error
	onStart func(union.Spec)

	// lchown is os.Lchown, replaced by a test: the paths a test prepares name
	// no real directory.
	lchown func(path string, uid, gid int) error

	// command builds the child. Nil in production, where it is union.Reexec of
	// Self; tests substitute a process that exits or stays up.
	command func(context.Context, union.Spec) *exec.Cmd
}

// live is one mounted union.
type live struct {
	spec   union.Spec
	host   string // the daemon this union is mounted in, for asking who holds it
	cache  string // the managed volume holding the layer, for the collector
	cancel context.CancelFunc
	done   chan struct{}

	// prepared is the Manager's prepares count when a Prepare last answered
	// with this union. Guarded by Manager.mu.
	prepared uint64

	// applied is what the client's fill and invalidations wrote through this
	// union, by path.
	//
	// The cache is written through the merged mount (ADR 0044), so the filled
	// copy of every file sits in the cache LAYER beside the container's own
	// writes, which is what Changes reads. Without this an idle session is
	// told about the whole tree every few seconds forever. The client's
	// manifest is the rule; this keeps the answer proportional, and losing it
	// on an agent restart costs one oversized reply and no correctness.
	appliedMu sync.Mutex
	applied   map[string]applied

	// exit is why the child last stopped and exits how many times it has, so
	// a Prepare still waiting for the mount can say so instead of waiting out
	// readyTimeout. Set by the supervisor after every attempt.
	exitMu sync.Mutex
	exit   error
	exits  int
}

// setExit records why the child stopped.
func (l *live) setExit(err error) {
	l.exitMu.Lock()
	l.exit = err
	l.exits++
	l.exitMu.Unlock()
}

// failed is why the child stopped, once it has stopped twice. One exit is
// absorbed by the supervisor's restart: the lower's NFS mount can be refused
// because the reverse forward is a moment late, and the second attempt works.
// Two in a row before the mount ever answered is a union that cannot start.
func (l *live) failed() error {
	l.exitMu.Lock()
	defer l.exitMu.Unlock()
	if l.exits < 2 {
		return nil
	}
	return l.exit
}

// applied is one file as the client's stream left it. ctime is its change
// time where the platform says, so a chmod or chown the container makes on a
// file the client sent is the container's change and not the client's (ADR
// 0044).
type applied struct {
	size    int64
	modTime time.Time
	ctime   int64
}

// noteApplied records what was just written through the union.
func (l *live) noteApplied(name string, a applied) {
	l.appliedMu.Lock()
	defer l.appliedMu.Unlock()
	if l.applied == nil {
		l.applied = map[string]applied{}
	}
	l.applied[name] = a
}

// forgetApplied drops a path, so a file the client removed and the container
// later recreates is reported.
func (l *live) forgetApplied(name string) {
	l.appliedMu.Lock()
	defer l.appliedMu.Unlock()
	delete(l.applied, name)
}

// isApplied reports whether an entry is exactly what the client last wrote. A
// ctime either side does not know is not compared.
func (l *live) isApplied(name string, size int64, modTime time.Time, ctime int64) bool {
	l.appliedMu.Lock()
	defer l.appliedMu.Unlock()
	a, ok := l.applied[name]
	return ok && a.size == size && a.modTime.Equal(modTime) &&
		(a.ctime == 0 || ctime == 0 || a.ctime == ctime)
}

// share is the live union for one of a machine's shares, if this manager has
// one.
func (m *Manager) share(account, client, export string) (*live, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.shares[key(account, client, export)]
	return l, ok
}

// relocate maps one of this union's paths, which is a path in the daemon's own
// filesystem, into the agent's view of it.
func (l *live) relocate(p string) (string, error) {
	return replay.Relocate(p, func() (string, error) { return l.spec.Root(), nil })
}

// chownUpper gives the union's upper directory, which is the merged root, to
// the owner a prepare names. Through the daemon's filesystem as the agent sees
// it, relocated and never following a link: the daemon's answers are untrusted
// (replay.Relocate).
func (m *Manager) chownUpper(l *live, want union.Spec) error {
	upper, err := l.relocate(want.Upper())
	if err != nil {
		return fmt.Errorf("unions: finding the cache layer of %s: %w", want.Export, err)
	}
	chown := os.Lchown
	if m.lchown != nil {
		chown = m.lchown
	}
	if err := chown(upper, int(want.UID), int(want.GID)); err != nil {
		return fmt.Errorf("unions: giving the root of %s to %d:%d: %w", want.Export, want.UID, want.GID, err)
	}
	return nil
}

// ErrNoShare is what every op answers for a share this workspace is not
// serving. Named so the client can stop asking rather than guess from a string.
var ErrNoShare = errors.New("no union for this share")

// key names a share within a machine of an account. Two accounts, or two of
// one account's machines, may name different directories alike (one path on
// two machines) and must never share a mount.
func key(account, client, export string) string {
	return owner(account, client) + export
}

// ownedBy reports whether a share key belongs to a machine, and exists so that
// only key() above knows how one is put together.
func ownedBy(k, account, client string) bool { return strings.HasPrefix(k, owner(account, client)) }

func owner(account, client string) string { return account + "\x00" + client + "\x00" }

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

	// Validated above, so this only fails for a request that skipped it.
	owner, err := req.OwnerOf()
	if err != nil {
		return "", fmt.Errorf("unions: %w", err)
	}

	spec := union.Spec{
		UID:      owner.UID,
		GID:      owner.GID,
		PID:      d.PID,
		Export:   req.Export,
		Client:   client,
		Port:     req.Port,
		Read:     read,
		CacheDir: cacheDir,
	}
	if err := spec.Validate(); err != nil {
		return "", err
	}

	k := key(account, client, req.Export)
	for {
		existing, known := m.share(account, client, req.Export)

		// Outside m.mu, for the reason on the pending field: a wedged server
		// takes aliveTimeout to say so.
		if known && m.alive(existing.spec) {
			// Idempotent for the owner the union has. A different one is
			// refused: the supervisor restarts the child from this spec, so
			// re-owning only the upper would be undone by the next restart,
			// and a container is using the mount meanwhile.
			if existing.spec.UID != spec.UID || existing.spec.GID != spec.GID {
				return "", fmt.Errorf("%s is cached for %d:%d and this prepare asks for %d:%d; one directory has one owner while a container uses it\n"+
					"  fix: give every mount of the directory the same owner, or remove the containers using it and restart the session",
					req.Export, existing.spec.UID, existing.spec.GID, spec.UID, spec.GID)
			}
			m.stamp(existing)
			return existing.spec.Merged(), nil
		}

		m.mu.Lock()
		if wait, ok := m.pending[k]; ok {
			m.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		// Another Prepare mounted this share and finished since it was looked
		// at above, so pending is empty again: ask about that union instead
		// of mounting over it.
		if m.shares[k] != existing {
			m.mu.Unlock()
			continue
		}
		wait := make(chan struct{})
		if m.pending == nil {
			m.pending = map[string]chan struct{}{}
		}
		m.pending[k] = wait
		m.mu.Unlock()

		merged, err := m.mount(ctx, k, d.Host, req.Cache, spec)

		m.mu.Lock()
		delete(m.pending, k)
		m.mu.Unlock()
		close(wait)

		return merged, err
	}
}

// mount replaces a share's union with a fresh one and waits for it to answer.
//
// The caller holds the share's pending entry rather than m.mu, so nothing else
// mounts this share meanwhile and no other share waits on this one.
func (m *Manager) mount(ctx context.Context, k, host, cacheVol string, spec union.Spec) (string, error) {
	m.mu.Lock()
	dead := m.shares[k]
	m.mu.Unlock()
	if dead != nil {
		// Down rather than absent, and torn out so the mount below is a fresh
		// one. A container already bound to the dead mount is not repaired by
		// this and cannot be.
		m.discard(k, dead)
	}

	m.mu.Lock()
	if m.shares == nil {
		m.shares = map[string]*live{}
	}
	l := m.start(spec, host)
	l.cache = cacheVol
	m.shares[k] = l
	m.mu.Unlock()

	if err := m.waitReady(ctx, spec, l); err != nil {
		m.discard(k, l)
		return "", err
	}
	// Also for a union ADOPTED rather than mounted by this child: its upper
	// was made by whichever agent mounted it, with whatever owner that one
	// was given.
	if err := m.chownUpper(l, spec); err != nil {
		// Discarded, or a union serving the wrong owner stays registered and
		// every later Prepare for the share is refused over it.
		m.discard(k, l)
		return "", err
	}
	m.stamp(l)
	return spec.Merged(), nil
}

// stamp marks a union as just handed to a Prepare (live.prepared).
func (m *Manager) stamp(l *live) {
	m.mu.Lock()
	m.prepares++
	l.prepared = m.prepares
	m.mu.Unlock()
}

// discard ends supervision of one union and waits for its supervisor, taking
// the lock only for the removal.
//
// A no-op unless the share still holds this exact union: ReleaseAccount can
// have taken it out while Prepare was waiting for it.
func (m *Manager) discard(k string, l *live) {
	m.mu.Lock()
	ours := m.shares[k] == l
	if ours {
		m.remove(k, l)
	}
	m.mu.Unlock()

	if ours {
		drain(l)
	}
}

// start runs the union's server and keeps running it.
func (m *Manager) start(spec union.Spec, host string) *live {
	ctx, cancel := context.WithCancel(context.Background())
	l := &live{spec: spec, host: host, cancel: cancel, done: make(chan struct{})}
	if m.onStart != nil {
		m.onStart(spec)
	}

	go func() {
		defer close(l.done)
		for ctx.Err() == nil {
			// Adopted rather than replaced (ADR 0044). After an agent
			// restart the child is an orphan whose mount is still serving
			// every container bound to it, and mounting over that strands
			// them: they keep the mount they have, and a second one stacked
			// on the path cannot repair it.
			//
			// Safe only because "alive" means MOUNTED rather than "the path
			// is there". Against a stat this would wait forever on the empty
			// directory a dead union leaves behind.
			if m.awaitGone(ctx, spec) || m.awaitGone(ctx, legacy(spec)) {
				continue
			}

			err := m.run(ctx, spec)
			if ctx.Err() != nil {
				return
			}
			if err == nil {
				err = errors.New("it exited without an error")
			}
			l.setExit(err)
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

// legacy is where an agent from before per-machine mountpoints put this share's
// union. One still serving after an upgrade may be on this share's upper, so it
// is waited out like any other, and a Prepare fails meanwhile rather than
// stacking a second union on that upper.
func legacy(spec union.Spec) union.Spec {
	spec.Client = ""
	return spec
}

// run is one attempt: the agent re-executed as the child that enters the
// daemon's namespace and becomes fuse-overlayfs.
func (m *Manager) run(ctx context.Context, spec union.Spec) error {
	var cmd *exec.Cmd
	if m.command != nil {
		cmd = m.command(ctx, spec)
	} else {
		cmd = union.Reexec(ctx, m.Self, union.ModeServe, spec)
	}
	// Both to stderr, as supervise.Dockerd does: what fuse-overlayfs says is
	// the only account of why a union failed, and losing it would leave a
	// share that does not work and says nothing. The tail is kept as well, so
	// a Prepare that finds the child gone can pass that account on.
	tail := &tailBuffer{max: tailBytes}
	w := io.MultiWriter(os.Stderr, tail)
	cmd.Stdout, cmd.Stderr = w, w

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("unions: starting the union for %s: %w", spec.Export, err)
	}
	if err := cmd.Wait(); err != nil {
		if msg := tail.report(); msg != "" {
			return errors.New(msg)
		}
		return err
	}
	return nil
}

// tailBytes bounds how much of the child's output is kept to report.
const tailBytes = 2048

// childProgram is how the agent's main prefixes the error a failed child
// exits with (cmd/remote-dockerd/main.go). It names the program, which the
// person reading the client's message never ran.
const childProgram = "remote-dockerd: "

// tailBuffer keeps what a child printed, to report why it stopped: the last
// max bytes, and separately the last error it printed, which a chatty program
// after it (fuse-overlayfs) must not push out.
type tailBuffer struct {
	mu   sync.Mutex
	max  int
	buf  []byte
	line []byte   // the line being written, not yet ended
	err  []string // the last error: its line, then the indented lines after it
	open bool     // the last complete line belongs to err
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.max:]...)
	}
	for _, b := range p {
		if b != '\n' {
			t.line = append(t.line, b)
			continue
		}
		t.endLine(strings.TrimRight(string(t.line), "\r"))
		t.line = t.line[:0]
	}
	return len(p), nil
}

func (t *tailBuffer) endLine(line string) {
	switch {
	case strings.HasPrefix(line, childProgram):
		t.err, t.open = []string{strings.TrimPrefix(line, childProgram)}, true
	case t.open && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")):
		t.err = append(t.err, line)
	default:
		t.open = false
	}
}

// report is the child's own diagnosis: its last error without the program
// name, then the indented fix: line after it. A child that printed none is
// reported by its last line.
func (t *tailBuffer) report() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.err) > 0 {
		return strings.Join(t.err, "\n")
	}
	lines := strings.Split(strings.TrimSpace(string(t.buf)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// waitReady blocks until the union answers, or gives up.
//
// The mount is what is waited for, not the process: fuse-overlayfs is up before
// its mount is, and a container bound to a mountpoint that is not yet a mount
// sees an empty directory rather than an error. A child that has exited
// twice in a row without the mount ever answering ends the wait with the
// last one's own words (the usual cause is fuse-overlayfs missing from the
// daemon's image). The first exit is left to the supervisor's restart. The
// caller then discards the union, which ends the supervisor: a union that
// failed here is mounted afresh by the next Prepare, not by a loop nobody is
// waiting on.
func (m *Manager) waitReady(ctx context.Context, spec union.Spec, l *live) error {
	started := time.Now()
	deadline := started.Add(readyTimeout)
	for {
		if m.alive(spec) {
			return nil
		}
		if err := l.failed(); err != nil {
			return fmt.Errorf("the union for %s stopped before it mounted: %w", spec.Export, err)
		}
		if time.Now().After(deadline) {
			// How long, because the budget is the likeliest thing to be wrong
			// and without the number this reads as a union that cannot work
			// rather than one that was not given time.
			return fmt.Errorf("the union for %s did not come up in %s",
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
	return m.aliveErr(context.Background(), spec) == nil
}

// aliveErr asks whether a union is serving, within aliveTimeout whatever the
// caller's context allows.
//
// The budget is not the caller's to skip: mergedRoot passes the cache session's
// context, which gliderlabs builds with context.WithCancel and no deadline
// (agent/internal/sshd/cache.go), so an Apply or a Drop against a wedged FUSE
// server waited for as long as the SSH session lived.
func (m *Manager) aliveErr(ctx context.Context, spec union.Spec) error {
	ctx, cancel := context.WithTimeout(ctx, aliveTimeout)
	defer cancel()

	if m.probe != nil {
		return m.probe(ctx, spec)
	}
	return union.Alive(ctx, spec)
}

// Open records a cache session of a machine, and returns what ends it: the
// machine's unused unions are released when its LAST session ends.
//
// A client replaces a channel it no longer trusts while the agent may still be
// finishing the old one, and a released connection can be followed by a new
// one at once. Releasing when any one session ended took the union another had
// just prepared, before its container existed to hold it: the container then
// bound an empty directory where the union should be (ADR 0044).
func (m *Manager) Open(account, client string) (end func(context.Context)) {
	o := owner(account, client)
	m.mu.Lock()
	if m.sessions == nil {
		m.sessions = map[string]int{}
	}
	m.sessions[o]++
	m.mu.Unlock()

	return func(ctx context.Context) {
		m.mu.Lock()
		if m.sessions[o]--; m.sessions[o] <= 0 {
			delete(m.sessions, o)
		}
		m.mu.Unlock()
		m.Release(ctx, account, client)
	}
}

// Release drops the shares a machine holds that nothing is using, which is
// what its last cache session ending means.
//
// NOT every share: the union outlives the channel that asked for it, because
// the channel goes whenever the connection under it is released (ADR 0015)
// while the containers bound to the union keep running (ADR 0044). Nothing
// while a session of the machine is open, and no union prepared while the
// daemon was being asked, whose container that answer cannot include: see Open.
func (m *Manager) Release(ctx context.Context, account, client string) {
	o := owner(account, client)
	m.mu.Lock()
	if m.sessions[o] > 0 {
		m.mu.Unlock()
		return
	}
	asked := m.prepares
	m.mu.Unlock()

	held := m.heldByContainers(ctx, account, client)

	m.mu.Lock()
	if m.sessions[o] > 0 {
		// That session's end releases them, asking the daemon afresh.
		m.mu.Unlock()
		return
	}
	var specs []union.Spec
	for k, l := range m.shares {
		if !ownedBy(k, account, client) || l.prepared > asked {
			continue
		}
		if held[l.spec.Merged()] {
			// Unmounting now frees nothing: the container keeps the mount it
			// already has, and keeps it BROKEN. Released instead the next time
			// the machine disconnects with nothing holding this share.
			continue
		}
		specs = append(specs, l.spec)
		m.remove(k, l)
		drain(l)
	}
	m.mu.Unlock()

	for _, spec := range specs {
		if err := union.Unmount(ctx, m.Self, spec); err != nil {
			logx.Or(m.Log).Warn("could not unmount a union", "export", spec.Export, "err", err)
		}
	}
}

// heldByContainers is the set of this machine's union mounts that a running
// container is still bound to.
//
// A union is bound into a container by PATH rather than as a volume, so nothing
// else in the workspace relates the two and the daemon is the only thing that
// can say. On any doubt this answers "held": keeping a mount nobody needs costs
// a process, while taking one that is in use breaks somebody's container
// permanently.
func (m *Manager) heldByContainers(ctx context.Context, account, client string) map[string]bool {
	m.mu.Lock()
	hosts := map[string]bool{}
	shares := map[string]bool{}
	for k, l := range m.shares {
		if ownedBy(k, account, client) {
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
				"account", account, "client", client, "err", err)
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
// Answered from the FILESYSTEM as well as from this process's own record,
// because a union outlives the agent that started it: after a restart the
// mounts are serving while this manager knows nothing about them, and a
// truthful "none mounted" costs somebody the contents of a cache their
// container is still reading. The share ids come from the mounts and the
// client digest from the key that authenticated, so only this machine's
// volumes are named. An error means the mounts could not be read, and every
// cache must be kept.
func (m *Manager) MountedCaches(account, client string, d Daemon) ([]string, error) {
	names := map[string]bool{}

	m.mu.Lock()
	for k, l := range m.shares {
		if ownedBy(k, account, client) && l.cache != "" {
			names[l.cache] = true
		}
	}
	m.mu.Unlock()

	if client != "" {
		ids, err := union.MountedShares(netns.Root(d.PID), client)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			names[workspace.VolumeNameForCache(client, id)] = true
		}
	}

	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// remove ends supervision and forgets the share. The caller holds the lock;
// waiting for the supervisor is drain's, and belongs outside it. ReleaseAccount
// still drains under the lock, which costs up to stopTimeout per share.
func (m *Manager) remove(k string, l *live) {
	l.cancel()
	delete(m.shares, k)
}

// drain waits for a stopped union's supervisor to finish, bounded so one wedged
// child cannot hold up every other share.
func drain(l *live) {
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
// runs (see daemons.DefaultImage), and the agent's own filesystem says nothing
// about it.
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
