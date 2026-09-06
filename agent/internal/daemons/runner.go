package daemons

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lhns/remote-docker/agent/internal/dockercli"
	"github.com/lhns/remote-docker/core-agent/netns"
	"github.com/lhns/remote-docker/core/logx"
)

// Daemon is one account's running dockerd.
type Daemon struct {
	Account string

	// PID is the daemon container's pid in the agent's own pid namespace.
	//
	// Reachable because the dind is a CHILD of the workspace's dockerd rather
	// than a sibling on the host: nested pid namespaces mean the pid docker
	// reports is one the agent can open under its own /proc. A sibling would
	// have a host pid that does not exist here, and making it exist would need
	// `pid: host`, so every enrolled user's shell would see every process on
	// the node. That is worse than the problem being solved.
	PID int

	// Socket is where the agent dials this daemon.
	Socket string

	// checked is when this daemon was last confirmed to be running. Guarded
	// by the manager's mutex, not by the daemon itself, because it is the
	// manager's bookkeeping rather than the daemon's own state.
	checked time.Time
}

// Host is this daemon as a DOCKER_HOST value, which is how everything but the
// agent addresses it: the docker CLI, and the shells the agent hands out.
func (d Daemon) Host() string { return HostFor(d.Account) }

// NetNSPath is the daemon's network namespace, for binding the reverse tunnel
// inside it and dialling published ports from it.
func (d Daemon) NetNSPath() string { return netns.Path(d.PID) }

// Root is the daemon's filesystem as seen from the agent, which is where a
// volume mountpoint reported by that daemon actually lives.
func (d Daemon) Root() string { return netns.Root(d.PID) }

// Manager starts, adopts and hands out per-account daemons.
type Manager struct {
	// Options are passed to Plan.
	Options Options

	// Log receives progress. Nil means silence.
	Log *slog.Logger

	// IDs resolves an account to the uid and gid that must own its socket.
	//
	// Injected, because this package cannot work it out. The unix user behind
	// an account is `rd-<account>` (ADR 0025) or, on a workspace older than
	// that, the bare name -- and the accounts store already holds the answer
	// for both. Deriving it here would put the naming rule in a second place,
	// which is how the socket came to be left owned by root: user.Lookup of
	// the ACCOUNT name found nothing, the chown never happened, and every
	// account got "permission denied" from its own daemon.
	//
	// Nil falls back to looking the account name up directly, which is what
	// the tests and an unprefixed workspace need.
	IDs func(account string) (uid, gid int, err error)

	// docker builds the client for a daemon: the workspace's own when the host
	// is empty, an account's when it is not. Nil is the real docker command,
	// and injecting it is what lets this file be tested without one.
	docker func(host string) docker

	mu      sync.Mutex
	byName  map[string]*Daemon
	pending map[string]chan struct{}
}

// docker is the part of the docker command this package uses.
type docker interface {
	Line(ctx context.Context, args ...string) (string, error)
	Run(ctx context.Context, what string, args ...string) error

	// Output is stdout and stderr together, for reading a container's log,
	// where the interesting half is usually stderr.
	Output(ctx context.Context, args ...string) ([]byte, error)
}

// realDocker is the docker command itself.
type realDocker struct{ cli dockercli.CLI }

func (r realDocker) Line(ctx context.Context, args ...string) (string, error) {
	return r.cli.Line(ctx, args...)
}

func (r realDocker) Run(ctx context.Context, what string, args ...string) error {
	return r.cli.Run(ctx, what, args...)
}

func (r realDocker) Output(ctx context.Context, args ...string) ([]byte, error) {
	return r.cli.Cmd(ctx, args...).CombinedOutput()
}

// client is the docker command for one daemon. An empty host is the parent.
func (m *Manager) client(host string) docker {
	if m.docker != nil {
		return m.docker(host)
	}
	return realDocker{dockercli.CLI{Host: host}}
}

// aliveTTL is how long a confirmed-running daemon is believed without asking
// again.
//
// Short enough that a daemon which died is noticed within a request or two,
// long enough that a burst of API calls, which is what any real docker
// command is, costs one check rather than hundreds.
const aliveTTL = 2 * time.Second

// DefaultReadyTimeout is how long a cold daemon has to answer.
//
// Generous because the first start of a dind on fuse-overlayfs is slow, and
// because the cost of being early is a session that fails for a reason the
// user cannot act on. The agent is the only thing that starts a daemon
// (ADR 0019), so the first account to ask pays for its own boot rather than
// finding one already warmed at workspace start: CI measured 90 seconds short.
const DefaultReadyTimeout = 180 * time.Second

// ensure returns the account's daemon, starting or restarting it if needed.
// Callers outside this package reach it through Ensure, which answers in the
// Target that both modes share.
//
// Idempotent and single-flighted per account. Several sessions for one user
// arriving together is the normal case, since the client opens one connection
// and the docker CLI opens more, and each starting its own daemon would be a
// race whose loser leaves a container behind.
func (m *Manager) ensure(ctx context.Context, account string) (*Daemon, error) {
	for {
		m.mu.Lock()
		d, known := m.byName[account]
		fresh := known && time.Since(d.checked) < aliveTTL
		m.mu.Unlock()

		// The common case by far, and it must cost nothing. EVERY Docker API
		// request from the client opens its own dial-stdio session and lands
		// here (`docker compose up` is hundreds), so a `docker inspect` per
		// call would add a subprocess to every request. Never check while the
		// manager's lock is held either: that serialises every account's
		// requests behind one exec.
		if fresh {
			return d, nil
		}
		if known {
			// Confirm it outside the lock, then record when we did.
			if m.alive(ctx, d) {
				m.mu.Lock()
				d.checked = time.Now()
				m.mu.Unlock()
				return d, nil
			}
		}

		m.mu.Lock()
		if wait, ok := m.pending[account]; ok {
			// Somebody else is starting it. Wait for them rather than racing.
			m.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		wait := make(chan struct{})
		if m.pending == nil {
			m.pending = map[string]chan struct{}{}
		}
		m.pending[account] = wait
		m.mu.Unlock()

		started, err := m.start(ctx, account)

		m.mu.Lock()
		delete(m.pending, account)
		if err == nil {
			if m.byName == nil {
				m.byName = map[string]*Daemon{}
			}
			started.checked = time.Now()
			m.byName[account] = started
		}
		m.mu.Unlock()
		close(wait)

		return started, err
	}
}

// Warm starts an account's daemon without waiting for it.
//
// Called when a key authenticates, so the boot hides behind the client's
// workspace-info round trip instead of behind its first docker command.
func (m *Manager) Warm(account string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), DefaultReadyTimeout)
		defer cancel()
		if _, err := m.Ensure(ctx, account); err != nil {
			m.log().Warn("warming a daemon", "account", account, "err", err)
		}
	}()
}

// start creates or restarts one account's daemon and waits for its socket.
func (m *Manager) start(ctx context.Context, account string) (*Daemon, error) {
	spec, err := Plan(account, m.Options)
	if err != nil {
		return nil, err
	}

	// The socket directory is bind-mounted into the daemon, so it has to
	// exist before the container starts or docker creates it root-owned with
	// the wrong mode. Two directories, two modes: SocketDir is 0755 so every
	// account can TRAVERSE it to its own subdirectory, and that subdirectory
	// is 0750 root:<account>, which is what keeps one account out of
	// another's daemon. A parent that is not traversable fails as "permission
	// denied while trying to connect to the Docker daemon socket" with
	// DOCKER_HOST set correctly. MkdirAll honours the umask; the Chmod does
	// not, and is the one that grants access.
	if err := os.MkdirAll(SocketDir, 0o755); err != nil {
		return nil, fmt.Errorf("daemons: preparing %s: %w", SocketDir, err)
	}
	if err := os.Chmod(SocketDir, 0o755); err != nil {
		return nil, fmt.Errorf("daemons: preparing %s: %w", SocketDir, err)
	}

	dir := filepath.Join(SocketDir, account)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("daemons: preparing %s: %w", dir, err)
	}

	// Created explicitly rather than as a side effect of `-v name:/path`, so
	// it carries labels: this volume holds everything the account owns, and
	// `docker volume ls --filter label=remote-docker.daemon` is how an
	// operator sees which volumes must never be pruned. Creating an existing
	// volume is a no-op.
	if err := m.parent().Run(ctx, "daemons: preparing "+account+"'s storage",
		"volume", "create",
		"--label", ManagedLabel+"=1",
		"--label", AccountLabel+"="+account,
		VolumeName(account)); err != nil {
		return nil, err
	}

	// Before anything decides to start it, so an upgrade is a redeploy rather
	// than a list of commands somebody has to be told.
	m.reconcile(ctx, account, spec)

	switch state := m.state(ctx, spec.Name); state {
	case "running":
		// Somebody else's Ensure won, or it survived our restart.
	case "":
		m.log().Info("starting a daemon", "account", account)
		if err := m.parent().Run(ctx, "daemons: starting "+spec.Name, spec.Args()...); err != nil {
			return nil, err
		}
	default:
		// Stopped, exited, created. START it rather than running a new one:
		// replacing the container would silently discard what it holds.
		m.log().Info("restarting an existing daemon", "account", account, "was", state)
		if err := m.parent().Run(ctx, "daemons: restarting "+spec.Name, "start", spec.Name); err != nil {
			return nil, err
		}
	}

	return m.await(ctx, account, spec.Name)
}

// await waits for the daemon to ANSWER, not for its socket file to exist.
//
// The difference is the whole function. dockerd binds its socket early and
// initialises its storage afterwards, so a daemon that dies on
// "several valid graphdrivers ... please cleanup" leaves a socket file behind
// looking exactly like a healthy one. Treating that as ready handed the client
// a socket nothing was listening on, and every command failed with a bare
//
//	error during connect: Get "http://.../_ping": EOF
//
// which names neither the daemon nor the reason.
//
// So readiness is a round trip: the socket exists, the container reports a pid,
// and the daemon answers a request. Only the last one is evidence.
func (m *Manager) await(ctx context.Context, account, name string) (*Daemon, error) {
	socket := SocketPathFor(account)
	deadline := time.Now().Add(DefaultReadyTimeout)

	for {
		if _, err := os.Stat(socket); err == nil {
			pid, cerr := m.pid(ctx, name)
			if cerr == nil && pid > 0 {
				d := &Daemon{Account: account, PID: pid, Socket: socket}
				// Chowned before the round trip: the agent is root and could
				// talk to a socket the account cannot, so asking first would
				// prove the wrong thing about it.
				if err := m.chown(account, socket); err != nil {
					m.log().Error("could not hand an account its socket", "account", account, "err", err)
				}
				if m.answers(ctx, d) {
					go m.warnIfSlowStorage(d)
					return d, nil
				}
			}
		}
		if time.Now().After(deadline) {
			// The daemon's own words, which are the only thing that says WHY.
			// Without them this is "did not answer", and the reason is in a
			// log the account cannot reach.
			return nil, fmt.Errorf("daemons: %s did not answer within %s.%s",
				name, DefaultReadyTimeout, m.lastWords(ctx, name))
		}
		select {
		case <-ctx.Done():
			return nil, m.gaveUp(ctx, name)
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// lastWordsTimeout bounds reading a failed daemon's log. Short: whatever it
// said, it said before it stopped.
const lastWordsTimeout = 5 * time.Second

// gaveUp names the daemon and carries its own last words, for when the
// CALLER's patience ran out rather than this loop's.
//
// Both budgets are the same duration, so the caller's context expires first and
// this is the path almost always taken. ctx.Err() alone is "context deadline
// exceeded", which names no daemon and no reason while one crash-loops.
func (m *Manager) gaveUp(ctx context.Context, name string) error {
	// A fresh context, because the caller's is spent and `docker logs` on a
	// cancelled one returns nothing at all.
	logCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lastWordsTimeout)
	defer cancel()

	return fmt.Errorf("daemons: %s did not start before the caller gave up: %w.%s",
		name, ctx.Err(), m.lastWords(logCtx, name))
}

// answers reports whether the daemon actually responds on its socket.
func (m *Manager) answers(ctx context.Context, d *Daemon) bool {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	_, err := m.client(d.Host()).Line(ctx, dockercli.ServerVersionArgs()...)
	return err == nil
}

// lastWords is the tail of a daemon's own log, for an error the account can
// act on.
//
// A per-account daemon that will not start is the one failure an account can
// neither diagnose nor fix: the log belongs to a daemon it deliberately cannot
// reach. Carrying a few lines back through the error is the difference between
// "did not answer" and "several valid graphdrivers: vfs, fuse-overlayfs;
// please cleanup".
func (m *Manager) lastWords(ctx context.Context, name string) string {
	out, err := m.parent().Output(ctx, "logs", "--tail", "8", name)
	if err != nil || len(bytes.TrimSpace(out)) == 0 {
		return ""
	}
	return " It last said:\n" + string(bytes.TrimSpace(out))
}

// alive reports whether a daemon we already handed out is still usable.
//
// Checked on every Ensure rather than trusted, because the container can go
// away underneath us (an OOM kill, an operator, a crash) and handing back a
// dead socket produces a connection error that names nothing.
//
// THE PID IS PART OF "usable". A daemon restarted by an operator, or by
// Ensure's `docker start` after it stopped, comes back as the same container
// with the same name, the same socket path and the same "running" status, and
// a DIFFERENT pid. Everything here that crosses into it goes through
// /proc/<pid>: the reverse tunnel carrying the client's NFS export (netns), and
// the volume mountpoints replay writes into (root). Against a stale pid those
// name a namespace that no longer exists, so the daemon answers Docker API
// calls perfectly while no container it starts can mount anything, and a
// client that requires its file server refuses to start at all, with nothing
// pointing at the daemon having restarted.
//
// Reported as not-alive rather than repaired in place: Ensure then goes through
// start, which finds the container already running and re-reads the pid, so the
// self-healing path is the one that already exists.
func (m *Manager) alive(ctx context.Context, d *Daemon) bool {
	if d == nil {
		return false
	}
	if _, err := os.Stat(d.Socket); err != nil {
		return false
	}

	// One inspect for both facts, which also halves what the hot path costs.
	out, err := m.inspect(ctx, ContainerName(d.Account), "{{.State.Status}} {{.State.Pid}}")
	if err != nil {
		return false
	}
	var status string
	var pid int
	if _, err := fmt.Sscanf(out, "%s %d", &status, &pid); err != nil {
		return false
	}
	if status != "running" || pid <= 0 {
		return false
	}
	if pid != d.PID {
		m.log().Info("a daemon restarted; re-reading it", "account", d.Account, "wasPID", d.PID, "pid", pid)
		return false
	}
	return true
}

// state is the container's state, or "" if there is no such container.
func (m *Manager) state(ctx context.Context, name string) string {
	out, err := m.inspect(ctx, name, "{{.State.Status}}")
	if err != nil {
		return ""
	}
	return out
}

func (m *Manager) pid(ctx context.Context, name string) (int, error) {
	out, err := m.inspect(ctx, name, "{{.State.Pid}}")
	if err != nil {
		return 0, err
	}
	var pid int
	if _, err := fmt.Sscanf(out, "%d", &pid); err != nil {
		return 0, fmt.Errorf("daemons: unreadable pid %q for %s", out, name)
	}
	return pid, nil
}

func (m *Manager) inspect(ctx context.Context, name, format string) (string, error) {
	return m.parent().Line(ctx, "inspect", name, "--format", format)
}

// parent is the workspace's own daemon, which hosts every per-account one.
func (m *Manager) parent() docker { return m.client("") }

// Adopt takes ownership of daemons left running by a previous agent.
//
// Called at startup. Without it a restarted agent would find every name taken
// and every user's running work unreachable, and `docker run --name` fails
// on a conflict rather than replacing, so it would stay that way.
//
// Deliberately NOT elevate's `docker rm -f <name>` opener. That is right for a
// singleton whose state is worthless and catastrophic for a daemon holding a
// user's containers.
func (m *Manager) Adopt(ctx context.Context) (int, error) {
	rows, err := m.managed(ctx)
	if err != nil {
		return 0, fmt.Errorf("daemons: listing daemons to adopt: %w", err)
	}

	adopted := 0
	for _, row := range rows {
		account := labelValue(row.Labels, AccountLabel)
		if account == "" {
			continue
		}

		// Registered, not merely counted. A daemon that is running but not in
		// the map is worse than one that is not running at all: Ensure would
		// find the name taken, `docker run --name` fails on a conflict rather
		// than replacing, and the account would be unable to use the daemon
		// holding its own containers.
		pid, err := m.pid(ctx, ContainerName(account))
		if err != nil || pid <= 0 {
			// Not running yet, and that is the ordinary case rather than a
			// problem. Either the daemon was stopped, or the parent dockerd
			// is still bringing it back after a restart: this runs the moment
			// the agent starts, which is a race it cannot win and does not
			// need to. Ensure does the work on demand.
			//
			// Left alone deliberately either way: starting every account's
			// daemon at boot would wake daemons for people who are not here.
			m.log().Info("an account has a daemon that is not running; it will start when they connect",
				"account", account)
			continue
		}

		m.mu.Lock()
		if m.byName == nil {
			m.byName = map[string]*Daemon{}
		}
		m.byName[account] = &Daemon{
			Account: account,
			PID:     pid,
			Socket:  SocketPathFor(account),
		}
		m.mu.Unlock()

		m.log().Info("adopted a running daemon", "account", account)
		adopted++
	}
	return adopted, nil
}

// labelValue reads one label out of docker's comma-separated rendering.
func labelValue(labels, key string) string {
	for _, l := range strings.Split(labels, ",") {
		if name, value, ok := strings.Cut(strings.TrimSpace(l), "="); ok && name == key {
			return value
		}
	}
	return ""
}

// chown hands the socket to the account.
//
// Re-applied on every start, not once: dockerd recreates the socket each time
// it boots, so ownership set at creation is gone after the first restart. The
// directory is 0750 and the socket 0660, so the account can reach its own
// daemon and nobody else can reach it at all, which is what lets the shared
// `docker` group go away.
func (m *Manager) chown(account, socket string) error {
	uid, gid, err := m.ids(account)
	if err != nil {
		return err
	}
	if err := os.Chown(filepath.Dir(socket), 0, gid); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Dir(socket), 0o750); err != nil {
		return err
	}
	if err := os.Chown(socket, uid, gid); err != nil {
		return err
	}
	return os.Chmod(socket, 0o660)
}

// ids resolves an account to the uid and gid that own its socket.
func (m *Manager) ids(account string) (int, int, error) {
	if m.IDs != nil {
		return m.IDs(account)
	}
	return lookupIDs(account)
}

// log is the manager's logger, or silence. See logx.Or.
func (m *Manager) log() *slog.Logger {
	return logx.Or(m.Log)
}

// lookup returns an account's daemon only if it is already running.
//
// Never starts one and never waits, which is the entire point: workspace-info
// is answered on the client's first round trip, and a cold dind takes seconds
// to boot. Blocking there would make every first connection look like a hang,
// and the client would be waiting for a version string it only displays.
func (m *Manager) lookup(ctx context.Context, account string) (*Daemon, bool) {
	m.mu.Lock()
	d, ok := m.byName[account]
	m.mu.Unlock()
	if !ok || !m.alive(ctx, d) {
		return nil, false
	}
	return d, true
}

// warnIfSlowStorage says so when a daemon came up on vfs.
//
// vfs has no copy-on-write: it copies the entire image on every container
// create. Nothing fails, so nothing is reported. `docker ps` stays instant
// while `docker create debian` takes a minute and a half, which reads as a
// hang rather than as a storage driver.
//
// dockerd chooses it silently when the graph filesystem refuses overlay2,
// which is exactly what a Ceph- or NFS-backed data directory does. The
// workspace's own dockerd is given --storage-driver=fuse-overlayfs for that
// reason, and a per-account daemon now inherits it, but a deployment can
// still arrive here, so it should arrive loudly.
// Runs on its own goroutine with its own context, because it is only a
// warning and it is reached while this account's start is holding the gate:
// every other request for the same account waits behind it, and `docker info`
// against a daemon that has just booted is not always quick.
func (m *Manager) warnIfSlowStorage(d *Daemon) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	driver, err := m.client(d.Host()).Line(ctx, "info", "--format", "{{.Driver}}")
	if err != nil || driver != "vfs" {
		return
	}
	m.log().Warn("a daemon is using the vfs storage driver, which copies the whole image on "+
		"every container create -- expect `docker run` to take minutes. Its storage is on a "+
		"filesystem that refused overlay2; set WORKSPACE_DIND_STORAGE_DRIVER (fuse-overlayfs "+
		"for Ceph- or NFS-backed data) and remove the container to rebuild it.",
		"account", d.Account, "container", ContainerName(d.Account))
}

// reconcile brings a daemon created from older settings up to date.
//
// The container is disposable and the graph volume is the data: removing and
// re-running the container keeps every image and container the account owns,
// which the suite proves by destroying it on purpose. So applying a new image,
// a new flag or a new mount is safe, as long as nothing is running inside.
//
// Two things it will NOT do on its own, and both matter:
//
//   - Nothing while the account has containers running. Recreating the daemon
//     stops them, and they do not come back unless they carry a restart policy.
//     A setting can wait; somebody's work cannot. It is applied the next time
//     that daemon is idle.
//   - Nothing when the STORAGE DRIVER is what changed. A graph written by one
//     driver cannot be read by another, so there is no recreation that keeps
//     the data. Discarding it or staying is the operator's choice, and
//     `remote-dockerd daemons reset` is how they make it.
func (m *Manager) reconcile(ctx context.Context, account string, spec Spec) {
	was, err := m.inspect(ctx, spec.Name, "{{index .Config.Labels \""+SpecLabel+"\"}}")
	if err != nil {
		// No such container: nothing to reconcile, it is about to be created.
		return
	}
	// A daemon from before this label existed is not evidence of drift, only
	// of age. Docker renders a missing label as "<no value>".
	if was == "<no value>" || was == Fingerprint(spec) {
		return
	}

	if m.storageChanged(ctx, spec.Name) {
		m.log().Warn("a daemon was created with a different storage driver. A graph written by "+
			"one driver cannot be read by another, so this cannot be applied without discarding "+
			"that account's images and containers. Run `remote-dockerd daemons reset <account>` "+
			"on this workspace to do it.", "account", account)
		return
	}

	if !m.idle(ctx, account, spec.Name) {
		return
	}

	m.log().Info("a daemon was created from older settings; recreating it "+
		"(its images and containers are on a volume and are kept)", "account", account)
	if err := m.parent().Run(ctx, "daemons: replacing "+spec.Name, "rm", "-f", spec.Name); err != nil {
		m.log().Error("could not replace a daemon", "container", spec.Name, "err", err)
	}
}

// storageChanged reports whether the graph driver is what differs, which is
// the one difference that cannot be applied by recreating the container.
func (m *Manager) storageChanged(ctx context.Context, name string) bool {
	was, err := m.inspect(ctx, name, "{{index .Config.Labels \""+StorageLabel+"\"}}")
	if err != nil || was == "<no value>" {
		return false
	}
	return was != m.Options.StorageDriver
}

// idle reports whether a daemon may be replaced without taking somebody's work
// with it.
//
// Two questions, and the order matters. A daemon that cannot say what it is
// running counts as busy, because the cost of being wrong is an account's
// containers. But one that is not RUNNING cannot be running anything, and
// asking it was how a crash-looping daemon counted as busy forever and was
// never replaced, under a log line that said "has containers running".
//
// So the parent is asked first: it can see the container from outside, which is
// what the account's own daemon cannot do for itself.
func (m *Manager) idle(ctx context.Context, account, name string) bool {
	// An empty state is no such container, which is nothing to protect either.
	if state := m.state(ctx, name); state != "running" {
		if state != "" {
			m.log().Info("a daemon is out of date and is not running; recreating it",
				"account", account, "state", state)
		}
		return true
	}

	if n := m.runningInside(ctx, account); n != 0 {
		m.log().Info("a daemon is out of date but has containers running; leaving it alone until it is idle",
			"account", account, "running", n)
		return false
	}
	return true
}

// managedRow is one of this workspace's daemon containers, as the parent daemon
// describes it.
type managedRow struct {
	Labels string `json:"Labels"`
	State  string `json:"State"`
}

// managed lists the daemon containers belonging to this workspace.
func (m *Manager) managed(ctx context.Context) ([]managedRow, error) {
	args := []string{"ps", "--all", "--no-trunc",
		"--filter", "label=" + ManagedLabel,
		"--format", "{{json .}}"}
	if m.Options.Workspace != "" {
		// Only ours. Another workspace's daemons on the same parent daemon are
		// not ours to restart, stop or reason about.
		args = append(args, "--filter", "label="+WorkspaceLabel+"="+m.Options.Workspace)
	}

	out, err := m.parent().Line(ctx, args...)
	if err != nil {
		return nil, err
	}

	var rows []managedRow
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		var row managedRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			continue
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// StopStrays stops per-account daemons that nothing is routing to.
//
// Called when the workspace serves one daemon for everybody (ADR 0012): every
// account is sent to the shared socket, so a daemon left from per-account mode
// answers nobody, and a broken one restarts forever with nothing supervising it.
//
// Only reachable on a VM (ADR 0025), where the agent restarts while the
// machine's dockerd keeps running. Compose and Swarm change the mode by
// recreating the workspace container, which restarts the parent dockerd, and a
// daemon with no restart policy stays down by itself.
//
// STOPPED, never removed: the volume behind the container holds that account's
// images and containers, and both come back if the mode changes back.
func (m *Manager) StopStrays(ctx context.Context) (int, error) {
	// Unfiltered, this would stop another workspace's daemons on a parent the
	// two share. Refused by name rather than left to the caller.
	if m.Options.Workspace == "" {
		return 0, errors.New("daemons: refusing to stop daemons without a workspace id to recognise our own")
	}

	rows, err := m.managed(ctx)
	if err != nil {
		return 0, fmt.Errorf("daemons: listing daemons to stop: %w", err)
	}

	stopped := 0
	for _, row := range rows {
		// "running" and "restarting" both, and restarting is the one that
		// matters: a daemon crash-looping in a mode that does not use it is
		// the case this exists for.
		if row.State != "running" && row.State != "restarting" {
			continue
		}
		account := labelValue(row.Labels, AccountLabel)
		if account == "" {
			continue
		}
		name := ContainerName(account)
		if err := m.parent().Run(ctx, "daemons: stopping "+name, "stop", name); err != nil {
			m.log().Warn("could not stop a daemon this mode does not use",
				"account", account, "container", name, "err", err)
			continue
		}
		m.log().Info("stopped a per-account daemon: this workspace serves one daemon for everybody, "+
			"so nothing routes to it. Its images and containers are kept.",
			"account", account, "container", name)
		stopped++
	}
	return stopped, nil
}

// runningInside counts the containers an account is running on its own daemon.
//
// Asked of the account's daemon rather than the parent, because these are its
// containers and only it can see them. A daemon that cannot be asked counts as
// busy: the cost of being wrong is somebody's work.
func (m *Manager) runningInside(ctx context.Context, account string) int {
	out, err := m.client(HostFor(account)).Line(ctx, "ps", "--quiet")
	if err != nil {
		return -1
	}
	if strings.TrimSpace(out) == "" {
		return 0
	}
	return len(strings.Split(strings.TrimSpace(out), "\n"))
}

// Reset removes an account's daemon so the next connection builds a fresh one.
//
// The container always goes; the graph volume only when asked. That split is
// the whole point: the container is disposable and recreating it keeps
// everything the account owns, so a reset that only replaces the container
// costs nothing. Purging is the other thing entirely: every image and
// container that account has. It is needed for one case, a change of storage
// driver, because a graph written by one driver cannot be
// read by another.
//
// An account is not asked to be offline first. Removing a daemon stops what it
// was running, which is why this is a command somebody runs rather than
// something the agent decides.
func (m *Manager) Reset(ctx context.Context, account string, purge bool) error {
	if _, err := Plan(account, m.Options); err != nil {
		return err
	}
	name := ContainerName(account)

	if err := m.parent().Run(ctx, "daemons: removing "+name, "rm", "-f", name); err != nil {
		// Not fatal on its own: the container may simply not exist, which is a
		// fine state to be in when the goal is for it not to exist.
		m.log().Error("removing a daemon", "container", name, "err", err)
	}

	m.mu.Lock()
	delete(m.byName, account)
	m.mu.Unlock()

	if !purge {
		return nil
	}
	return m.parent().Run(ctx, "daemons: removing "+account+"'s storage",
		"volume", "rm", VolumeName(account))
}

// Accounts lists the accounts that currently have a daemon, running or not.
func (m *Manager) Accounts(ctx context.Context) ([]string, error) {
	rows, err := m.managed(ctx)
	if err != nil {
		return nil, fmt.Errorf("daemons: listing daemons: %w", err)
	}

	var accounts []string
	for _, row := range rows {
		if a := labelValue(row.Labels, AccountLabel); a != "" {
			accounts = append(accounts, a)
		}
	}
	sort.Strings(accounts)
	return accounts, nil
}
