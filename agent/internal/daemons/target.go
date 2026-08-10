package daemons

import "context"

// Target is where one account's Docker daemon is, in the four ways the agent
// needs to address it.
//
// It exists because the agent used to ask that question at nine separate call
// sites, each with its own `if manager == nil` for the shared-daemon mode. The
// answer is one thing, and a wrong answer here does not fail: it succeeds,
// against another account's containers. So it is resolved once, through
// Targets, and every site takes what it is given.
type Target struct {
	// Socket is the unix socket a `docker system dial-stdio` session is
	// spliced to. This is the Docker API as the client sees it.
	Socket string

	// Host is the same daemon as a DOCKER_HOST value, for the shells the agent
	// hands out and for the times it shells out to the docker binary.
	//
	// EMPTY means "whatever docker would use by default", which is what the
	// shared daemon wants: /var/run/docker.sock is already right, and setting
	// DOCKER_HOST to it in a login shell would only be noise.
	Host string

	// NetNSPath is the network namespace to bind reverse tunnels in and dial
	// published ports from.
	//
	// EMPTY means the agent's own, which is where the shared daemon lives. See
	// netns.Do: the empty path is what lets both modes share one code path.
	NetNSPath string

	// Root is this daemon's filesystem as seen from the agent, which is where
	// a volume mountpoint it reports actually lives. "/" for a daemon sharing
	// our filesystem; /proc/<pid>/root for one in a container of its own.
	//
	// Untrusted input downstream: the daemon reports its own mountpoints and
	// the account is root inside it. See notify.relocate, which checks that a
	// joined path stays under the root, because path.Join CLEANS and ".." escapes
	// look like containment.
	Root string
}

// Targets resolves an account to its daemon.
//
// Two lookups, and the difference between them is load-bearing rather than
// stylistic:
//
//   - Ensure waits, starting the daemon if it is not up. Right for anything
//     the user is waiting on: a docker command, a shell, a forward.
//   - Lookup never waits. Right for the answers folded into workspace-info,
//     which is the client's FIRST round trip: a cold daemon must not turn
//     every new connection into a boot-length hang for a version string the
//     client only displays.
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

// Target satisfies Targets for the per-account manager.
func (m *Manager) target(d *Daemon) Target {
	return Target{
		Socket:    d.Socket,
		Host:      d.Host(),
		NetNSPath: d.NetNSPath(),
		Root:      d.Root(),
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
func (m *Manager) Mode() string { return ModePerAccount }

// The two modes, named once. They travel to the client in workspace-info and
// are printed by `remote-docker status`.
const (
	ModeShared     = "shared"
	ModePerAccount = "per-account"
)

// shared is the workspace's own dockerd, serving every account (ADR 0012).
//
// A Targets implementation rather than a nil check, which is the whole point:
// the mode is chosen once, where it is read from the environment, and no code
// downstream asks again. It was nine `if Daemons != nil` branches, and the
// invariant they were guarding is one that fails by succeeding.
type shared struct{ socket string }

// Shared serves every account from one daemon at the given socket.
func Shared(socket string) Targets {
	if socket == "" {
		socket = "/var/run/docker.sock"
	}
	return shared{socket: socket}
}

// The same target for everybody, which is exactly what this mode means. The
// account is accepted and ignored rather than absent from the signature: it is
// what makes the two modes the same call.
func (s shared) Ensure(_ context.Context, _ string) (Target, error) {
	return s.target(), nil
}

func (s shared) Lookup(_ context.Context, _ string) (Target, bool) {
	return s.target(), true
}

// Nothing to warm: it is already running, and it is the daemon the agent
// itself uses.
func (shared) Warm(string) {}

func (shared) Mode() string { return ModeShared }

// Host and NetNSPath are deliberately empty, not filled in with the values
// that would be equivalent. Empty is the statement that no redirection is
// needed (the default socket, this namespace) and it is what the call
// sites branch on where they still need to (a login shell gets no DOCKER_HOST
// rather than a redundant one).
func (s shared) target() Target {
	return Target{Socket: s.socket, Root: "/"}
}
