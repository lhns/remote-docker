// Package machine provisions the Linux system a workspace runs on, for a
// machine that has none.
//
// The workspace itself is unchanged: once the machine exists, it is reached
// over SSH and serves files back over NFS exactly as a workspace on another
// continent does. What this package adds is a lifecycle, not a second data
// path (ADR 0026).
//
// Nobody working on this project has WSL or Hyper-V, so every decision here is
// a pure function of a string and the platform calls are behind Backend. The
// _windows.go files are all that is left outside that: wsl_windows.go runs in
// CI on a Windows runner, and hyperv_windows.go runs nowhere at all.
package machine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
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

	// Image is the workspace image this machine runs, by full reference: the
	// same artifact the container deployment uses and CI builds on every push,
	// which is what makes "nothing is installed at provisioning time" true.
	Image string

	// Rootfs is where that image's filesystem comes from: a local path or a
	// URL.
	//
	// Not derived from Image here. Getting a rootfs out of a reference needs
	// either a docker daemon, which is the thing being installed, or a registry
	// client, and the caller is where that decision belongs.
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
	// afterwards through Enrol.
	//
	// Deliberately NOT part of Generation. A rotated key would otherwise mean a
	// rebuild deciding itself, and a rebuild discards every image in the
	// machine (ADR 0026).
	PublicKey string
}

// Generation identifies a Spec, so a machine built from older settings can be
// recognised without inspecting it.
//
// The same trick agent/internal/daemons.reconcile uses for per-account
// daemons: the settings are hashed, the hash is stored with the thing, and a
// mismatch is a fact rather than a guess.
//
// Truncated to 16 hex characters. It identifies a local machine against its own
// configuration and is not a security boundary.
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
	// Separate from Create so that rotating a key costs nothing. Spec.PublicKey
	// says why it is not part of the generation; hyperVEnrolment is the backend
	// that can only report a mismatch rather than write a key.
	Enrol(ctx context.Context, name, account, publicKey string) error
	Start(ctx context.Context, name string) error

	// Hold keeps the machine from going away, until the returned Closer is
	// closed.
	//
	// A machine with nobody in it shuts down, and WSL counts only its own
	// sessions as somebody: neither an open TCP connection from the host nor a
	// command that runs and exits is one. Poking every ten seconds was measured
	// (ADR 0026) starting a machine that stopped thirty seconds later, so its
	// dockerd never became ready and its agent never opened a listener.
	//
	// So the hold is one session that STAYS OPEN, and it is the caller's job to
	// keep it for as long as the machine is needed.
	Hold(ctx context.Context, name string) (io.Closer, error)

	// Address is where this machine can be reached from here.
	//
	// Asked every time, never stored. A local machine is on a virtual network
	// whose address it is given at boot, so a stored one is wrong from the
	// moment the machine restarts: the connection is refused, or worse, reaches
	// whatever has the address now (ADR 0026).
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
// Here rather than in a _windows.go file beside the backend that needed it
// first: locate_test.go's fake backend returns a hold too, and a helper
// compiled only on Windows makes that test compile only on Windows.
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
// Both halves are why this exists, and neither is optional. A machine nobody
// is using goes away, so a workspace on one cannot be dialled the way a
// workspace on another host is: that host is always there and the machine is
// not. Starting is also what makes the address answerable, since a stopped
// machine has none.
func Locate(ctx context.Context, backendName, name string, port int) (string, error) {
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

	// "Located" has to mean "dialable". A machine that was stopped is up before
	// its agent is, since the agent generates a host key and waits for dockerd
	// before it listens, so returning the address at boot hands the caller a
	// refused connection that works on the next attempt. Here rather than in
	// each of the three callers because the one that forgot was the session.
	if err := waitForListener(ctx, addr, port); err != nil {
		return "", fmt.Errorf("the %s machine %q is running but %w", backendName, name, err)
	}
	return addr, nil
}

// waitForListener blocks until something accepts a connection at the address.
//
// A dial rather than a handshake: this asks whether the listener is open, and
// anything further is the session's job to report properly.
func waitForListener(ctx context.Context, host string, port int) error {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	deadline := time.Now().Add(agentStartTimeout)

	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("its agent is not answering on %s", addr)
}

// agentStartTimeout is how long the agent has to open its listener.
//
// Longer than the agent's own ninety-second wait for dockerd, deliberately:
// the agent serves anyway once that expires, so a client waiting the same
// ninety seconds would give up at the exact moment the agent starts answering.
var agentStartTimeout = 3 * time.Minute

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
