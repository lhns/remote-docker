// Package machine provisions the Linux system a workspace runs on, for a
// machine that has none.
//
// The workspace itself is unchanged: once the machine exists, it is reached
// over SSH and serves files back over NFS exactly as a workspace on another
// continent does. There is no second data path, and that is the point -- what
// this package adds is a lifecycle, not a second product.
//
// The decisions here are pure and the platform calls are behind Backend, which
// is the shape agent/internal/elevate and agent/internal/daemons already use.
// It matters more here than it did there: nobody working on this project has
// WSL or Hyper-V, so anything that is not a pure function is code that ships
// without having run.
package machine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Spec is what a machine should be, derived entirely from configuration.
//
// Entirely is the load-bearing word. Everything needed to build the machine is
// here, so building it twice from the same Spec gives the same machine, and
// rebuilding is the ordinary path run again rather than a repair mode. There is
// no package manager on any path and nothing is installed at provisioning time:
// the unit of change is a published artifact named below.
type Spec struct {
	// Name is the machine's name on the host, which is not the workspace's
	// name: two workspaces could reasonably be called "dev" on one machine.
	Name string

	// Backend selects the implementation. See Backends.
	Backend string

	// Image is the workspace image this machine runs, by full reference. It is
	// the artifact the whole design rests on: the same one the container
	// deployment uses and CI builds on every push.
	Image string

	// Rootfs is where the workspace image's filesystem comes from: a local
	// path or a URL.
	//
	// A container image IS a rootfs, so this is the same artifact the container
	// deployment runs and CI builds on every push -- which is what makes
	// "nothing is installed at provisioning time" true rather than aspirational.
	// It is not derived from Image here: getting a rootfs out of a reference
	// needs either a docker daemon, which is the thing being installed, or a
	// registry client, and the caller is where that decision belongs.
	Rootfs string

	// CPUs and MemoryMB are what the machine is given. Zero means the
	// backend's own default, because a number invented here would be worse
	// than the one the platform already chose.
	CPUs     int
	MemoryMB int

	// Port is where the agent's SSH listener is reachable on this host.
	Port int

	// Account is the workspace account this machine's owner logs in as.
	Account string

	// PublicKey is the key that account logs in with.
	//
	// Part of the Spec because one backend needs it at creation: a Hyper-V
	// machine has no door but the SSH this key opens, so the key goes into the
	// Ignition document or it never gets in at all. The WSL backend writes it
	// afterwards through Enrol, which is why this is not the only path.
	//
	// Deliberately NOT part of Generation. A rotated key would otherwise mean a
	// rebuild deciding itself, and a rebuild discards every image in the
	// machine (ADR 0026).
	PublicKey string
}

// Generation identifies a Spec, so a machine built from older settings can be
// recognised without inspecting it.
//
// The same trick daemons.reconcile uses for per-account daemons: the settings
// are hashed, the hash is stored with the thing, and a mismatch is a fact
// rather than a guess. It is what turns "the install is somehow broken" into a
// state with a name.
//
// Truncated to 16 hex characters. This identifies a local machine against its
// own configuration; it is not a security boundary and the full digest would
// only make the error messages harder to read.
func (s Spec) Generation() string {
	// Written out field by field rather than through a struct encoder, so that
	// adding a field to Spec and forgetting it here is a compile error at the
	// call below rather than a generation that silently stops changing.
	parts := []string{
		"name=" + s.Name,
		"backend=" + s.Backend,
		"image=" + s.Image,
		"rootfs=" + s.Rootfs,
		fmt.Sprintf("cpus=%d", s.CPUs),
		fmt.Sprintf("memory=%d", s.MemoryMB),
		fmt.Sprintf("port=%d", s.Port),
		"account=" + s.Account,
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])[:16]
}

// State is what a machine currently is.
type State int

const (
	// Absent means the backend has no machine by that name.
	Absent State = iota

	// Stopped means it exists and is not running.
	Stopped

	// Running means it exists and is running.
	Running
)

func (s State) String() string {
	switch s {
	case Absent:
		return "absent"
	case Stopped:
		return "stopped"
	case Running:
		return "running"
	default:
		return "unknown"
	}
}

// Observed is what a backend found.
type Observed struct {
	State State

	// Generation is the Spec the machine was built from, or "" when the
	// backend cannot tell. An unknown generation is treated as a match rather
	// than as a mismatch: recreating somebody's machine because we could not
	// read a label would destroy their work to satisfy our bookkeeping.
	Generation string
}

// Action is what to do about the difference between a Spec and what is there.
type Action int

const (
	// Nothing means it exists, is running, and matches.
	Nothing Action = iota

	// Create means there is no machine.
	Create

	// Start means it exists and matches but is not running.
	Start

	// Recreate means it exists and was built from different settings.
	Recreate
)

func (a Action) String() string {
	switch a {
	case Nothing:
		return "nothing"
	case Create:
		return "create"
	case Start:
		return "start"
	case Recreate:
		return "recreate"
	default:
		return "unknown"
	}
}

// Plan says what to do to make the machine match the spec.
//
// Pure, and the only place the rules live. Whether a mismatch is acted on is
// the CALLER's decision: `machine start` reports one and refuses, because
// silently destroying a machine somebody has containers in is not a thing a
// start command should do, while `machine rebuild` asks for it.
func Plan(spec Spec, observed Observed) Action {
	if observed.State == Absent {
		return Create
	}
	if observed.Generation != "" && observed.Generation != spec.Generation() {
		return Recreate
	}
	if observed.State == Stopped {
		return Start
	}
	return Nothing
}

// Backend is one way of making a Linux system on this host.
//
// Small on purpose. Everything above this line is testable anywhere; every
// method below it is code that only runs on a platform none of the people
// writing it have.
type Backend interface {
	// Name is the value that appears in configuration as `backend`.
	Name() string

	// Available reports why this backend cannot be used here, or nil.
	//
	// An error rather than a bool: "Hyper-V is not enabled on this edition of
	// Windows" and "WSL is not installed" are different problems with
	// different fixes, and a caller that can only say "unavailable" makes the
	// user find out which.
	Available(ctx context.Context) error

	Inspect(ctx context.Context, name string) (Observed, error)
	Create(ctx context.Context, spec Spec) error

	// Enrol makes a public key able to log in as an account.
	//
	// Separate from Create so that rotating a key costs nothing. It is not part
	// of the generation for the same reason: a new key would otherwise mean a
	// rebuild, and a rebuild discards every image in the machine -- a heavy
	// price for a thing people are supposed to do often.
	Enrol(ctx context.Context, name, account, publicKey string) error
	Start(ctx context.Context, name string) error

	// Hold keeps the machine from going away, until the returned Closer is
	// closed.
	//
	// A machine with nobody in it shuts down, and what counts as somebody is
	// not what you would expect: WSL counts its own sessions, so a TCP
	// connection from the host does not, and neither does a command that runs
	// and exits. Poking one every ten seconds was measured failing exactly that
	// way -- the machine started, ran for about thirty seconds, stopped, and
	// started again on the next poke, so its dockerd never got far enough to be
	// ready and its agent never opened a listener.
	//
	// So the hold is one session that stays open, and it is the caller's job to
	// keep it for as long as the machine is needed.
	Hold(ctx context.Context, name string) (io.Closer, error)

	// Address is where this machine can be reached from here.
	//
	// Asked rather than assumed, and asked every time. A local machine is on a
	// virtual network whose address it is given at boot, so the answer changes
	// when it restarts and a stored one goes stale silently -- the connection
	// is refused, or worse, reaches whatever has the address now.
	Address(ctx context.Context, name string) (string, error)

	Stop(ctx context.Context, name string) error
	Destroy(ctx context.Context, name string) error
}

// namePrefix keeps our machines out of the user's own namespace.
//
// A WSL distribution list and a Hyper-V VM list are both places the user has
// their own things, and `Get-VM dev` or `wsl -d dev` are poor names to take
// from somebody. Same argument as the unix account prefix (ADR 0025), and the
// same prefix, so one machine is spelled the same way everywhere it appears.
const namePrefix = "rd-"

// machineName is what a machine is called on the platform hosting it.
func machineName(name string) string { return namePrefix + name }

// stateDir is where a machine's disk and configuration live.
//
// One function rather than one per backend: they differ in what they put there,
// never in where it goes, and two copies of a path is two answers to "what does
// `rm` delete".
func stateDir(name string) (string, error) {
	local := os.Getenv("LOCALAPPDATA")
	if local == "" {
		return "", errors.New("LOCALAPPDATA is not set, so there is nowhere to put the machine's disk")
	}
	return filepath.Join(local, "remote-docker", "machines", name), nil
}

// firstIPv4 picks an address to reach a machine at, out of whatever the
// platform reported.
//
// Link-local (169.254/16) is skipped rather than returned: it means DHCP has
// not finished, so the machine is up and not ready. Returning it produces a
// connection error naming an address nobody recognises, where returning nothing
// makes the caller wait, which is the correct thing to do about a machine that
// is still starting.
func firstIPv4(fields []string) string {
	for _, f := range fields {
		f = strings.TrimSpace(f)
		// A prefix length is the machine's, not ours: 172.24.110.158/20.
		if i := strings.IndexByte(f, '/'); i >= 0 {
			f = f[:i]
		}
		if strings.Count(f, ".") != 3 || strings.HasPrefix(f, "169.254.") {
			continue
		}
		return f
	}
	return ""
}

// closerFunc makes a func into an io.Closer.
//
// Here rather than beside the backend that first needed it, because a test is
// a platform too: a fake backend has to return a hold, and a helper compiled
// only on Windows makes the test compile only on Windows -- which is how it
// would go unrun on the machine it was written on and fail in CI.
type closerFunc func() error

func (f closerFunc) Close() error { return f() }

// Hold keeps a machine alive until the returned Closer is closed. See
// Backend.Hold.
func Hold(ctx context.Context, backendName, name string) (io.Closer, error) {
	backend, err := Find(backendName)
	if err != nil {
		return nil, err
	}
	return backend.Hold(ctx, name)
}

// Locate starts a machine if it is stopped and returns where to reach it.
//
// Both halves are why this exists, and neither is optional. A machine that
// nobody is using goes away -- WSL shuts an idle distribution down, and a TCP
// connection from the host is not use it counts -- so a workspace on a machine
// cannot be dialled the way a workspace on another host is: the host is always
// there and the machine is not. Starting is also what makes the address
// answerable, since a stopped machine has none.
func Locate(ctx context.Context, backendName, name string) (string, error) {
	backend, err := Find(backendName)
	if err != nil {
		return "", err
	}
	// Start rather than Inspect-then-start: starting a running machine is what
	// keeps it running, and there is no window between the two answers.
	if err := backend.Start(ctx, name); err != nil {
		return "", fmt.Errorf("starting the %s machine %q: %w", backendName, name, err)
	}
	addr, err := backend.Address(ctx, name)
	if err != nil {
		return "", err
	}
	if addr == "" {
		return "", fmt.Errorf("the %s machine %q has no address yet", backendName, name)
	}
	return addr, nil
}

// Backends returns the backends compiled into this build, by name.
//
// Populated per platform. On anything but Windows this is empty, and the error
// from Find says so rather than pretending a backend exists and failing later
// with something obscure.
func Backends() map[string]Backend {
	out := make(map[string]Backend, len(registered))
	for _, b := range registered {
		out[b.Name()] = b
	}
	return out
}

// registered is filled by the platform files' init.
var registered []Backend

// Find returns the named backend.
func Find(name string) (Backend, error) {
	available := Backends()
	if b, ok := available[name]; ok {
		return b, nil
	}

	names := make([]string, 0, len(available))
	for n := range available {
		names = append(names, n)
	}
	sort.Strings(names)

	if len(names) == 0 {
		return nil, fmt.Errorf(
			"no machine backend is available on this platform\n" +
				"  fix: a machine is provisioned on Windows; elsewhere, point a workspace at a Linux host with `remote create`")
	}
	return nil, fmt.Errorf("no machine backend named %q; this build has: %s",
		name, strings.Join(names, ", "))
}
