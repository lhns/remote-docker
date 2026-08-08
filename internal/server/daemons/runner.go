package daemons

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Daemon is one account's running dockerd.
type Daemon struct {
	Account string

	// Container is the id of the dind holding it.
	Container string

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
}

// NetNSPath is the daemon's network namespace, for binding the reverse tunnel
// inside it and dialling published ports from it.
func (d Daemon) NetNSPath() string { return fmt.Sprintf("/proc/%d/ns/net", d.PID) }

// Root is the daemon's filesystem as seen from the agent, which is where a
// volume mountpoint reported by that daemon actually lives.
func (d Daemon) Root() string { return fmt.Sprintf("/proc/%d/root", d.PID) }

// Manager starts, adopts and hands out per-account daemons.
type Manager struct {
	// Docker is the CLI used to talk to the WORKSPACE's own daemon, which is
	// the parent of every per-account one. Empty means "docker" on PATH.
	Docker string

	// Options are passed to Plan.
	Options Options

	// ReadyTimeout bounds how long Ensure waits for a new daemon's socket to
	// appear. Zero means DefaultReadyTimeout.
	ReadyTimeout time.Duration

	// Log receives progress. Nil means silence.
	Log func(format string, args ...any)

	mu      sync.Mutex
	byName  map[string]*Daemon
	pending map[string]chan struct{}
}

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
		if d, ok := m.byName[account]; ok && m.alive(ctx, d) {
			m.mu.Unlock()
			return d, nil
		}
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

		d, err := m.start(ctx, account)

		m.mu.Lock()
		delete(m.pending, account)
		if err == nil {
			if m.byName == nil {
				m.byName = map[string]*Daemon{}
			}
			m.byName[account] = d
		}
		m.mu.Unlock()
		close(wait)

		return d, err
	}
}

// Warm starts an account's daemon without waiting for it.
//
// Called when a key authenticates, so the boot hides behind the client's
// workspace-info round trip instead of behind its first docker command.
func (m *Manager) Warm(account string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), m.readyTimeout())
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
	dir := filepath.Join(SocketDir, account)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("daemons: preparing %s: %w", dir, err)
	}

	switch state := m.state(ctx, spec.Name); state {
	case "running":
		// Somebody else's Ensure won, or it survived our restart.
	case "":
		m.logf("starting a daemon for %s", account)
		if out, err := m.docker(ctx, spec.Args()...).CombinedOutput(); err != nil {
			return nil, fmt.Errorf("daemons: starting %s: %w: %s",
				spec.Name, err, strings.TrimSpace(string(out)))
		}
	default:
		// Stopped, exited, created. START it rather than running a new one:
		// the container holds this user's containers and images, and
		// replacing it would silently discard them.
		m.logf("restarting the existing daemon for %s (was %s)", account, state)
		if out, err := m.docker(ctx, "start", spec.Name).CombinedOutput(); err != nil {
			return nil, fmt.Errorf("daemons: restarting %s: %w: %s",
				spec.Name, err, strings.TrimSpace(string(out)))
		}
	}

	return m.await(ctx, account, spec.Name)
}

// await waits for the daemon's socket to appear and reads back its pid.
func (m *Manager) await(ctx context.Context, account, name string) (*Daemon, error) {
	socket := SocketPathFor(account)
	deadline := time.Now().Add(m.readyTimeout())

	for {
		if _, err := os.Stat(socket); err == nil {
			pid, cerr := m.pid(ctx, name)
			if cerr == nil && pid > 0 {
				id, _ := m.inspect(ctx, name, "{{.Id}}")
				d := &Daemon{Account: account, Container: id, PID: pid, Socket: socket}
				if err := m.chown(account, socket); err != nil {
					m.logf("could not hand %s its socket: %v", account, err)
				}
				return d, nil
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("daemons: %s did not answer within %s", name, m.readyTimeout())
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
	out, err := m.docker(ctx, "inspect", name, "--format", format).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

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

	out, err := m.docker(ctx, args...).Output()
	if err != nil {
		return 0, fmt.Errorf("daemons: listing daemons to adopt: %w", err)
	}

	adopted := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
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
		m.logf("adopting the daemon for %s", account)
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

func (m *Manager) docker(ctx context.Context, args ...string) *exec.Cmd {
	docker := m.Docker
	if docker == "" {
		docker = "docker"
	}
	return exec.CommandContext(ctx, docker, args...)
}

func (m *Manager) readyTimeout() time.Duration {
	if m.ReadyTimeout > 0 {
		return m.ReadyTimeout
	}
	return DefaultReadyTimeout
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
