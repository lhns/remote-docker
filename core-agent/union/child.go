package union

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"sort"
	"strconv"
	"sync"

	"github.com/lhns/remote-docker/core-agent/netns"
	"github.com/lhns/remote-docker/core/workspace"
)

// How the agent hands a spec to the child it re-executes.
//
// The environment rather than argv, for one reason worth stating: `ps` on a
// workspace shows every account's mounts, and an export path is the one field
// here that names somebody's directory. Neither is a secret, but argv is the
// more public of the two and nothing is gained by putting it there.
const (
	envMode   = "RD_UNION_MODE"
	envPID    = "RD_UNION_PID"
	envExport = "RD_UNION_EXPORT"
	envPort   = "RD_UNION_PORT"
	envCache  = "RD_UNION_CACHE"
	envRead   = "RD_UNION_READ"
)

// What the child is being asked to do.
const (
	// ModeServe prepares the layers and becomes fuse-overlayfs. It does not
	// return.
	ModeServe = "serve"

	// ModeUnmount takes the union down. A separate run; see Release.
	ModeUnmount = "unmount"
)

// envFor encodes a spec, and what to do with it, for the child.
func envFor(mode string, spec Spec) []string {
	return []string{
		envMode + "=" + mode,
		envPID + "=" + strconv.Itoa(spec.PID),
		envExport + "=" + spec.Export,
		envPort + "=" + strconv.Itoa(spec.Port),
		envCache + "=" + spec.CacheDir,
		envRead + "=" + string(spec.Read),
	}
}

// FromEnv reads back what Env wrote, and reports which mode was asked for.
//
// Validated here as well as where it was built. The child runs as root inside
// somebody's daemon, and "the parent checked" is not a property this side can
// see.
func FromEnv(getenv func(string) string) (Spec, string, error) {
	pid, err := strconv.Atoi(getenv(envPID))
	if err != nil {
		return Spec{}, "", fmt.Errorf("union: %s: %w", envPID, err)
	}
	port, err := strconv.Atoi(getenv(envPort))
	if err != nil {
		return Spec{}, "", fmt.Errorf("union: %s: %w", envPort, err)
	}

	spec := Spec{
		PID:      pid,
		Export:   getenv(envExport),
		Port:     port,
		CacheDir: getenv(envCache),
		Read:     workspace.Read(getenv(envRead)),
	}
	if err := spec.Validate(); err != nil {
		return Spec{}, "", err
	}

	mode := getenv(envMode)
	switch mode {
	case ModeServe, ModeUnmount:
		return spec, mode, nil
	default:
		return Spec{}, "", fmt.Errorf("union: %s=%q is not a mode", envMode, mode)
	}
}

// Root is the daemon's filesystem as the agent can read it, which is how the
// agent inspects and writes into a mount it cannot enter. See netns.Root.
func (s Spec) Root() string { return netns.Root(s.PID) }

// errNotAMount is what a path that exists and is not a mount reports. Named
// rather than an os error, because "no such file or directory" about a
// directory that is plainly there is the message this whole check exists to
// stop being given.
var errNotAMount = errors.New("nothing is mounted there")

// prober answers whether a union is serving, with at most one Lstat in flight
// per merged path.
//
// mountedAt's Lstat blocks uninterruptibly on a wedged FUSE server, so it
// outlives the context that gave up waiting for it and pins an OS thread until
// it returns. A caller arriving meanwhile learns nothing from a second one:
// "has not answered yet" is already the answer. Unbounded, unions.awaitGone
// polls this every restartDelay for the whole life of an adopted mount and
// leaks a goroutine and a thread every two seconds, forever.
//
// The zero value is ready, and it is safe for concurrent use.
type prober struct {
	mu       sync.Mutex
	inflight map[string]*probe

	// mounted is mountedAt, indirected so a test can hold a probe open. Set at
	// construction and never reassigned: the goroutine below reads it, so a
	// later write races with a probe still blocked in the previous one. Nil in
	// production, since there is no way to wedge a FUSE server on a
	// development machine.
	mounted func(string) bool
}

// at reports whether anything is mounted at path.
func (p *prober) at(path string) bool {
	if p.mounted != nil {
		return p.mounted(path)
	}
	return mountedAt(path)
}

// probe is one Lstat and whatever it eventually answered. Waiters read err
// after done closes.
type probe struct {
	done chan struct{}
	err  error
}

// defaultProber is the process's, because what is being bounded is an OS thread
// and there is one pool of those.
var defaultProber prober

// Alive reports whether the union answers, and it is the ONLY definition of
// "up" this package offers.
//
// The MOUNT is the truth, not the process: an orphaned server still serves, and
// a live one can hold a mount that answers ENOTCONN. What "mounted" means, and
// why it is not a stat, is on mountedAt. Read through /proc/<pid>/root, which
// resolves in the daemon's namespace without entering it.
//
// A context because a wedged server answers nothing at all: the Lstat blocks on
// the FUSE server behind it, so it runs on a goroutine of its own and every
// caller asking does not wait with it. Bounding that goroutine is prober's.
func Alive(ctx context.Context, spec Spec) error {
	return defaultProber.alive(ctx, spec)
}

// alive is Alive against this prober's in-flight set.
func (p *prober) alive(ctx context.Context, spec Spec) error {
	merged := path.Join(spec.Root(), spec.Merged())

	pr, ours := p.begin(merged)
	if ours {
		go func() {
			var err error
			if !p.at(merged) {
				err = errNotAMount
			}
			p.finish(merged, pr, err)
		}()
	}

	select {
	case <-pr.done:
		if pr.err != nil {
			return fmt.Errorf("union: %s is not serving: %w", spec.Export, pr.err)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("union: %s did not answer: %w", spec.Export, ctx.Err())
	}
}

// begin joins the probe already in flight for merged, or claims the right to
// make one.
func (p *prober) begin(merged string) (*probe, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pr, ok := p.inflight[merged]; ok {
		return pr, false
	}
	if p.inflight == nil {
		p.inflight = map[string]*probe{}
	}
	pr := &probe{done: make(chan struct{})}
	p.inflight[merged] = pr
	return pr, true
}

// finish publishes an answer and clears the way for the next probe.
//
// Cleared BEFORE the answer goes out, so an Lstat that returns after minutes of
// blocking is never handed to a later caller as a fresh reading: that caller
// starts its own.
func (p *prober) finish(merged string, pr *probe, err error) {
	pr.err = err

	p.mu.Lock()
	if p.inflight[merged] == pr {
		delete(p.inflight, merged)
	}
	p.mu.Unlock()

	close(pr.done)
}

// MountedShares names the share ids that have a union mounted, reading the
// filesystem under root rather than any process's memory.
//
// Which is the point: after an agent restart the mounts are still serving and
// nothing in this process knows about them. Anything that decides what may be
// deleted has to ask the filesystem, or it will truthfully report "none" about
// unions that are running (ADR 0044).
//
// root is "/" for the shared daemon and /proc/<pid>/root for one per account,
// exactly as Spec.Root gives it.
func MountedShares(root string) []string {
	entries, err := os.ReadDir(path.Join(root, Root))
	if err != nil {
		// No union directory at all is no unions, which is the ordinary case
		// on a workspace that has never served one.
		return nil
	}

	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if mountedAt(path.Join(root, Root, e.Name(), "merged")) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// Reexec is the agent run again as the child that does the work: self is the
// agent's own binary, and mode is what the child is asked to do. The caller
// decides what happens to its output and how long it is supervised.
func Reexec(ctx context.Context, self, mode string, spec Spec) *exec.Cmd {
	cmd := exec.CommandContext(ctx, self, Command)
	cmd.Env = append(os.Environ(), envFor(mode, spec)...)
	return cmd
}

// Unmount takes a union down, through a child; see Release.
func Unmount(ctx context.Context, self string, spec Spec) error {
	if out, err := Reexec(ctx, self, ModeUnmount, spec).CombinedOutput(); err != nil {
		return fmt.Errorf("union: unmounting %s: %w: %s", spec.Export, err, out)
	}
	return nil
}
