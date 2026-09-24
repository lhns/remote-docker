package daemons

import (
	"context"

	"github.com/lhns/remote-docker/core/workspace"
)

// Target is where one account's Docker daemon is, in every way the agent needs
// to address it. Resolve it once, through Targets, and let every site take what
// it is given.
type Target struct {
	// Socket is the unix socket a `docker system dial-stdio` session is
	// spliced to. This is the Docker API as the client sees it.
	Socket string

	// Host is the same daemon as a DOCKER_HOST value, for the shells the agent
	// hands out and the docker binary it runs. EMPTY for the shared daemon: the
	// default socket is already right.
	Host string

	// NetNSPath is the network namespace to bind reverse tunnels in and dial
	// published ports from. EMPTY is the agent's own; see netns.Do.
	NetNSPath string

	// Root is this daemon's filesystem as seen from the agent, where a volume
	// mountpoint it reports lives: "/" or /proc/<pid>/root. What it reports is
	// untrusted, since the account is root inside it; see replay.Relocate.
	Root string

	// PID is the process whose mount namespace a union is mounted INSIDE (ADR
	// 0044), which Root, a path to read through, cannot express. ZERO is the
	// agent's own.
	PID int
}

// Targets resolves an account to its daemon, and is the ONE way that is done:
// daemons.Shared (ADR 0012) or a *Manager (ADR 0019), chosen once where the
// mode is read. Never an `if manager == nil` at a use site; see CLAUDE.md, "An
// account is resolved to its daemon exactly once".
//
//   - Ensure waits, starting the daemon: anything the user is waiting on.
//   - Lookup never waits: the answers folded into workspace-info, the client's
//     FIRST round trip, which a cold daemon must not turn into a boot-length
//     hang for a version string.
type Targets interface {
	Ensure(ctx context.Context, account string) (Target, error)
	Lookup(ctx context.Context, account string) (Target, bool)

	// Warm starts an account's daemon in the background, without waiting.
	// Called when a key authenticates, so the boot hides behind the client's
	// first round trip rather than behind its first docker command.
	Warm(account string)

	// Mode names the arrangement for workspace-info, so a client and an
	// operator can both see which one they are on.
	Mode() string
}

// target renders one daemon as the Target both modes answer in.
func (m *Manager) target(d *Daemon) Target {
	return Target{
		Socket:    d.Socket,
		Host:      d.Host(),
		NetNSPath: d.NetNSPath(),
		Root:      d.Root(),
		PID:       d.PID,
	}
}

// Ensure resolves an account to its own daemon, starting it if needed.
func (m *Manager) Ensure(ctx context.Context, account string) (Target, error) {
	d, err := m.ensure(ctx, account)
	if err != nil {
		return Target{}, err
	}
	return m.target(d), nil
}

// Lookup resolves an account to its daemon only if it is already running.
func (m *Manager) Lookup(ctx context.Context, account string) (Target, bool) {
	d, ok := m.lookup(ctx, account)
	if !ok {
		return Target{}, false
	}
	return m.target(d), true
}

// Mode names this arrangement in workspace-info.
func (m *Manager) Mode() string { return workspace.ModePerAccount }

// shared is the workspace's own dockerd, serving every account (ADR 0012): an
// implementation rather than a nil check, for the reason on Targets.
type shared struct{ socket string }

// Shared serves every account from one daemon at the given socket.
func Shared(socket string) Targets {
	if socket == "" {
		socket = "/var/run/docker.sock"
	}
	return shared{socket: socket}
}

// The same target for everybody, which is what this mode means.
func (s shared) Ensure(_ context.Context, _ string) (Target, error) {
	return s.target(), nil
}

func (s shared) Lookup(_ context.Context, _ string) (Target, bool) {
	return s.target(), true
}

// Nothing to warm: it is already running, and it is the daemon the agent
// itself uses.
func (shared) Warm(string) {}

func (shared) Mode() string { return workspace.ModeShared }

// Host and NetNSPath empty, meaning no redirection; see Target.
func (s shared) target() Target {
	return Target{Socket: s.socket, Root: "/"}
}
