package daemons

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lhns/remote-docker/internal/server/dockercli"
	"github.com/lhns/remote-docker/internal/server/netns"
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
	// `pid: host` -- every enrolled user's shell seeing every process on the
	// node, which is worse than the problem being solved.
	PID int

	// Socket is where the agent dials this daemon.
	Socket string

	// checked is when this daemon was last confirmed to be running. Guarded
	// by the manager's mutex, not by the daemon itself, because it is the
	// manager's bookkeeping rather than the daemon's own state.
	checked time.Time
}

// Host is this daemon as a DOCKER_HOST value, which is how everything that is
// not the agent itself addresses it -- the docker CLI, and the shells the
// agent hands out.
func (d Daemon) Host() string { return HostFor(d.Account) }

// NetNSPath is the daemon's network namespace, for binding the reverse tunnel
// inside it and dialling published ports from it.
func (d Daemon) NetNSPath() string { return netns.Path(d.PID) }

// Root is the daemon's filesystem as seen from the agent, which is where a
// volume mountpoint reported by that daemon actually lives.
func (d Daemon) Root() string { return fmt.Sprintf("/proc/%d/root", d.PID) }

// Manager starts, adopts and hands out per-account daemons.
type Manager struct {
	// Options are passed to Plan.
	Options Options

	// Log receives progress. Nil means silence.
	Log func(format string, args ...any)

	mu      sync.Mutex
	byName  map[string]*Daemon
	pending map[string]chan struct{}
}

// aliveTTL is how long a confirmed-running daemon is believed without asking
// again.
//
// Short enough that a daemon which died is noticed within a request or two,
// long enough that a burst of API calls -- which is what any real docker
// command is -- costs one check rather than hundreds.
const aliveTTL = 2 * time.Second

// DefaultReadyTimeout is how long a cold daemon has to answer.
//
// Generous because the first start of a dind on fuse-overlayfs is slow, and
// because the cost of being early is a session that fails for a reason the
// user cannot act on.
const DefaultReadyTimeout = 90 * time.Second

// Ensure returns the account's daemon, starting or restarting it if needed.
//
// Idempotent and single-flighted per account: several sessions for one user
// arriving together is the normal case -- the client opens one connection and
// the docker CLI opens more -- and each starting its own daemon would be a
// race whose loser leaves a container behind.
func (m *Manager) Ensure(ctx context.Context, account string) (*Daemon, error) {
	for {
		m.mu.Lock()
		d, known := m.byName[account]
		fresh := known && time.Since(d.checked) < aliveTTL
		m.mu.Unlock()

		// The common case by far, and it must cost nothing. EVERY Docker API
		// request from the client opens its own dial-stdio session and lands
		// here -- `docker compose up` is hundreds -- so a `docker inspect`
		// per call would add a subprocess to every request. Worse, the check
		// used to run while the manager's lock was HELD, which serialised
		// every account's requests behind one exec.
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
			m.logf("warming %s: %v", account, err)
		}
	}()
}

// start creates or restarts one account's daemon and waits for its socket.
func (m *Manager) start(ctx context.Context, account string) (*Daemon, error) {
	spec, err := Plan(account, m.Options)
	if err != nil {
		return nil, err
	}

	// The socket directory is ours, not the daemon's: it is bind-mounted in,
	// so it has to exist before the container starts or docker creates it
	// root-owned with the wrong mode.
	// Two directories, two modes, and the difference is the whole point.
	//
	// SocketDir must be TRAVERSABLE by every account (0755): it holds one
	// subdirectory per account, and a shell has to walk through it to reach
	// its own socket. MkdirAll made both levels 0750 root:root and only the
	// leaf was ever chowned, so every account's DOCKER_HOST pointed at a path
	// it could not enter -- "permission denied while trying to connect to the
	// Docker daemon socket", with the variable set correctly.
	//
	// The per-account directory below it is 0750 root:<account>, which is what
	// actually keeps one account out of another's daemon. Traversing the
	// parent reveals only the names of directories nobody else may enter.
	if err := os.MkdirAll(SocketDir, 0o755); err != nil {
		return nil, fmt.Errorf("daemons: preparing %s: %w", SocketDir, err)
	}
	// MkdirAll honours the umask; this does not, and this is the one that
	// grants access.
	if err := os.Chmod(SocketDir, 0o755); err != nil {
		return nil, fmt.Errorf("daemons: preparing %s: %w", SocketDir, err)
	}

	dir := filepath.Join(SocketDir, account)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("daemons: preparing %s: %w", dir, err)
	}

	// Created explicitly rather than implicitly by `-v name:/path`, so it
	// carries labels. A volume docker creates as a side effect has none, and
	// this is the one object in the whole design that must never be deleted by
	// mistake: it holds everything the account owns. `docker volume ls
	// --filter label=remote-docker.daemon` is the difference between an
	// operator being able to see that and having to know it.
	//
	// Idempotent: creating an existing volume is a no-op that returns its name.
	if err := m.parent().Run(ctx, "daemons: preparing "+account+"'s storage",
		"volume", "create",
		"--label", ManagedLabel+"=1",
		"--label", AccountLabel+"="+account,
		VolumeName(account)); err != nil {
		return nil, err
	}

	switch state := m.state(ctx, spec.Name); state {
	case "running":
		// Somebody else's Ensure won, or it survived our restart.
	case "":
		m.logf("starting a daemon for %s", account)
		if err := m.parent().Run(ctx, "daemons: starting "+spec.Name, spec.Args()...); err != nil {
			return nil, err
		}
	default:
		// Stopped, exited, created. START it rather than running a new one:
		// the container holds this user's containers and images, and
		// replacing it would silently discard them.
		m.logf("restarting the existing daemon for %s (was %s)", account, state)
		if err := m.parent().Run(ctx, "daemons: restarting "+spec.Name, "start", spec.Name); err != nil {
			return nil, err
		}
	}

	return m.await(ctx, account, spec.Name)
}

// await waits for the daemon's socket to appear and reads back its pid.
func (m *Manager) await(ctx context.Context, account, name string) (*Daemon, error) {
	socket := SocketPathFor(account)
	deadline := time.Now().Add(DefaultReadyTimeout)

	for {
		if _, err := os.Stat(socket); err == nil {
			pid, cerr := m.pid(ctx, name)
			if cerr == nil && pid > 0 {
				d := &Daemon{Account: account, PID: pid, Socket: socket}
				if err := m.chown(account, socket); err != nil {
					m.logf("could not hand %s its socket: %v", account, err)
				}
				m.warnIfSlowStorage(ctx, d)
				return d, nil
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("daemons: %s did not answer within %s", name, DefaultReadyTimeout)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// alive reports whether a daemon we already handed out is still usable.
//
// Checked on every Ensure rather than trusted, because the container can go
// away underneath us -- an OOM kill, an operator, a crash -- and handing back a
// dead socket produces a connection error that names nothing.
func (m *Manager) alive(ctx context.Context, d *Daemon) bool {
	if d == nil {
		return false
	}
	if _, err := os.Stat(d.Socket); err != nil {
		return false
	}
	return m.state(ctx, ContainerName(d.Account)) == "running"
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
func (m *Manager) parent() dockercli.CLI { return dockercli.CLI{} }

// Adopt takes ownership of daemons left running by a previous agent.
//
// Called at startup. Without it a restarted agent would find every name taken
// and every user's running work unreachable -- and `docker run --name` fails
// on a conflict rather than replacing, so it would stay that way.
//
// Deliberately NOT elevate's `docker rm -f <name>` opener. That is right for a
// singleton whose state is worthless and catastrophic for a daemon holding a
// user's containers.
func (m *Manager) Adopt(ctx context.Context) (int, error) {
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
		return 0, fmt.Errorf("daemons: listing daemons to adopt: %w", err)
	}

	adopted := 0
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		var row struct {
			Names  string `json:"Names"`
			Labels string `json:"Labels"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			continue
		}
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
			// problem. Two ways it happens: the daemon was stopped, or the
			// parent dockerd is still bringing it back after a restart -- this
			// runs the moment the agent starts, which is a race it cannot win
			// and does not need to. Ensure does the work on demand.
			//
			// Left alone deliberately either way: starting every account's
			// daemon at boot would wake daemons for people who are not here.
			m.logf("%s has a daemon that is not running; it will start when they connect", account)
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

		m.logf("adopted the running daemon for %s", account)
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
// daemon and nobody else can reach it at all -- which is what lets the shared
// `docker` group go away.
func (m *Manager) chown(account, socket string) error {
	uid, gid, err := lookupIDs(account)
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

func (m *Manager) logf(format string, args ...any) {
	if m.Log != nil {
		m.Log(format, args...)
	}
}

// Lookup returns an account's daemon only if it is already running.
//
// Never starts one and never waits, which is the entire point: workspace-info
// is answered on the client's first round trip, and a cold dind takes seconds
// to boot. Blocking there would make every first connection look like a hang,
// and the client would be waiting for a version string it only displays.
func (m *Manager) Lookup(ctx context.Context, account string) (*Daemon, bool) {
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
// create. Nothing fails, so nothing is reported -- `docker ps` stays instant
// while `docker create debian` takes a minute and a half, which reads as a
// hang rather than as a storage driver.
//
// dockerd chooses it silently when the graph filesystem refuses overlay2,
// which is exactly what a Ceph- or NFS-backed data directory does. The
// workspace's own dockerd is given --storage-driver=fuse-overlayfs for that
// reason, and a per-account daemon now inherits it -- but a deployment can
// still arrive here, so it should arrive loudly.
func (m *Manager) warnIfSlowStorage(ctx context.Context, d *Daemon) {
	driver, err := dockercli.CLI{Host: d.Host()}.Line(ctx, "info", "--format", "{{.Driver}}")
	if err != nil || driver != "vfs" {
		return
	}
	m.logf("WARNING: %s's daemon is using the vfs storage driver, which copies "+
		"the whole image on every container create -- expect `docker run` to take "+
		"minutes. Its storage is on a filesystem that refused overlay2; set "+
		"WORKSPACE_DIND_STORAGE_DRIVER (fuse-overlayfs for Ceph- or NFS-backed "+
		"data) and remove %s to rebuild it.", d.Account, ContainerName(d.Account))
}
