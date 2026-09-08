// Package daemons gives each enrolled account its own Docker daemon, behind
// the same single SSH port (ADR 0019).
//
// Say this out loud, because it will otherwise be believed: THIS IS
// SEPARATION, NOT ISOLATION. Each per-user daemon runs privileged, and
// privileged is root on whatever hosts it, so a determined user can still
// reach another's. What changes is that nobody sees anyone else's work by
// accident. Genuine isolation is still one workspace container per account.
//
// The plan is pure and lives next to the runner for the same reason elevate's
// does: the difference between a correct and a catastrophic invocation is one
// flag, and it belongs in a test rather than in a command line.
package daemons

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/lhns/remote-docker/agent/internal/elevate"
)

// ErrUnsupported is returned by the parts that need a unix account database.
// The agent is Linux-only; this exists so the module still builds on the
// development machine.
var ErrUnsupported = errors.New("daemons: per-account daemons are Linux-only")

// DefaultImage is the dind image each account's daemon runs when nothing else
// is known.
//
// A LAST RESORT: stock docker:dind does not carry fuse-overlayfs, so a daemon
// inheriting that storage driver on this image restart-loops with
//
//	exec: "fuse-overlayfs": executable file not found in $PATH
//
// The right image is the workspace's OWN, which carries what this project
// decided it needs; see elevate.ImageEnv.
const DefaultImage = "docker:28-dind"

// Entrypoint is what a per-account daemon runs.
//
// Set explicitly because the image is the workspace's own, whose entrypoint is
// the agent. Left alone, the daemon container would run `remote-dockerd`
// handed dockerd's flags.
//
// It is dind's OWN entrypoint script, not `dockerd`, because the script does
// setup dockerd does not do for itself: removing a stale /var/run/docker.pid.
// Without it the FIRST start works and every RESTART dies with
//
//	failed to start daemon, ensure docker is not running or delete
//	/var/run/docker.pid: process with PID 1 is still running
//
// so a daemon looks fine until the workspace is restarted, which is exactly
// when nobody is watching. Both candidate images have it, since the
// workspace's own is built FROM docker:dind. What the script does with its
// FIRST ARGUMENT is on the command Plan builds.
const Entrypoint = "dockerd-entrypoint.sh"

// SocketDir is where the agent keeps one socket directory per account, and
// SocketMount is where that directory appears inside the account's daemon.
//
// Deliberately not /var/run: binding over it inside the dind would hide
// containerd's own sockets. The daemon still listens on /var/run/docker.sock
// as well, so anything inside expecting the usual path finds it.
const (
	SocketDir   = "/run/rd"
	SocketMount = "/rd-sock"
	SocketName  = "docker.sock"
)

// ExecRoot is dockerd's exec-root inside a daemon container, given a tmpfs of
// its own so that nothing in it survives the container being restarted. This
// is where the whole reason lives; supervise.prepareExecRoot does the same for
// the shared daemon and points here.
//
// A container's /run is part of its writable layer, so runtime state written
// there outlives a kill, which on a real machine it never does. dind's
// entrypoint deletes `docker*.pid` and containerd's file is `containerd.pid`,
// so a daemon killed rather than stopped comes back with a stale
// containerd/containerd.pid naming a pid from its previous life. That kills
// the daemon two ways, and a fix answering only the first is half a fix:
//
//   - dockerd starts containerd, refuses to record its pid over a file naming a
//     live process, kills what it just started and exits with
//     `failed to save daemon pid to disk: process with PID 35 is still running`
//   - or it reads that pid, believes containerd is already up, starts nothing
//     and times out 15s later waiting for it
//
// containerd boots in 0.01s, so neither is a budget that was too short. The
// pid is live only by coincidence, which is why this presented as a flake.
const ExecRoot = "/var/run/docker"

// Labels identify a container as a daemon we manage, and whose.
//
// The workspace label carries an id persisted in the state directory, NOT the
// container id. A container id changes on every redeploy, so adopting by it
// would orphan every user's daemon the first time somebody ran
// `docker compose up -d`, leaving them running, unreferenced, and holding
// their users' containers.
const (
	ManagedLabel   = "remote-docker.daemon"
	AccountLabel   = "remote-docker.account"
	WorkspaceLabel = "remote-docker.workspace"

	// SpecLabel and StorageLabel record what a daemon was CREATED with: the
	// whole spec digested, and the graph driver on its own.
	//
	// A daemon that already exists is STARTED, never re-run, which is what
	// keeps an account's containers and images across a redeploy, and means
	// its command line is fixed for life. Without a record of what it was
	// created from, a changed setting silently applies to nobody who already
	// has a daemon, which on any workspace that has been used is everybody.
	// The driver is separate because it is the one change that cannot be
	// applied by recreating the container; see Manager.reconcile.
	SpecLabel    = "remote-docker.spec"
	StorageLabel = "remote-docker.storage-driver"
)

// Spec describes one account's daemon.
type Spec struct {
	Name       string
	Image      string
	Entrypoint string
	Privileged bool

	// No --rm and no restart policy, and this is where somebody will look for
	// them. `--rm` deletes the containers, images and volumes a user's daemon
	// holds the moment it stops; a restart policy makes the parent dockerd a
	// second supervisor with no backoff and nothing in our log (ADR 0019).
	// Ensure starts a daemon when its account connects, and that is the whole
	// lifecycle. plan_test.go pins both flags absent.

	Labels []string
	Mounts []elevate.Mount
	Env    []string

	// Tmpfs are paths that must start empty every time. See ExecRoot.
	Tmpfs []string

	// Command is the dockerd invocation, including the sockets it listens on.
	Command []string
}

// Options tune the plan.
type Options struct {
	// Image overrides the dind image.
	Image string

	// Workspace is the persisted id of this workspace, used to adopt our own
	// daemons after a restart and to ignore anybody else's.
	Workspace string

	// Mounts are added to every account's daemon, on top of the two it always
	// has, for configuration the daemon can only be given as files. Parsed
	// from WORKSPACE_DIND_MOUNTS by ParseMounts, which says what for.
	Mounts []elevate.Mount

	// StorageDriver is passed to the per-user dockerd. Not inherited from the
	// parent, so it has to be stated: see StorageDriverFrom.
	StorageDriver string
}

// ContainerName is what one account's daemon is called.
//
// Derived from the account rather than from a random id, so a daemon can be
// found again after the agent restarts even before its labels are read.
func ContainerName(account string) string {
	return "rd-dind-" + account
}

// VolumeName is where one account's /var/lib/docker lives.
//
// A named volume on the WORKSPACE's daemon, so it lands on a real filesystem
// rather than on an overlay (overlay2 on overlay2 never arises) and so it
// survives the daemon being restarted, the agent being restarted and the
// workspace being redeployed. Never collected automatically, for the same
// reason accounts are revoked rather than deleted: the cost of being wrong is
// somebody's work.
func VolumeName(account string) string {
	return "rd-dind-" + account + "-lib"
}

// SocketPathFor is where the agent dials one account's daemon.
func SocketPathFor(account string) string {
	return SocketDir + "/" + account + "/" + SocketName
}

// HostFor is that same socket as a DOCKER_HOST value.
func HostFor(account string) string {
	return "unix://" + SocketPathFor(account)
}

// Plan works out what to launch for one account. Pure, so every rule here is
// testable on a machine with no daemon.
func Plan(account string, opts Options) (Spec, error) {
	if account == "" {
		return Spec{}, fmt.Errorf("daemons: no account to plan for")
	}
	if strings.ContainsAny(account, "/ \t") {
		return Spec{}, fmt.Errorf("daemons: account %q is not a usable container name component", account)
	}

	image := opts.Image
	if image == "" {
		image = DefaultImage
	}

	// `dockerd` FIRST, and that word is the only thing keeping this daemon off
	// TCP. dind's entrypoint supplies dockerd's own --host flags whenever there
	// is no argument or the first starts with a dash, and one of them is always
	// tcp://0.0.0.0:2375: an unauthenticated Docker API in the account's own
	// network namespace, reachable by every container it runs, that nothing
	// here has ever dialled. Naming the binary skips that block and keeps the
	// one below it, which deletes a stale docker*.pid, injects tini and sets up
	// iptables. (docker-library/docker `dockerd-entrypoint.sh`, read
	// 2026-09-08; re-check with `curl -s https://raw.githubusercontent.com/docker-library/docker/master/dockerd-entrypoint.sh`.)
	//
	// Two listeners, the one the agent dials and the conventional path, so
	// anything running inside the daemon's own container still works.
	command := []string{
		"dockerd",
		"-H", "unix://" + SocketMount + "/" + SocketName,
		"-H", "unix:///var/run/docker.sock",
	}
	if opts.StorageDriver != "" {
		command = append(command, "--storage-driver", opts.StorageDriver)
	}

	labels := []string{
		ManagedLabel + "=1",
		AccountLabel + "=" + account,
		StorageLabel + "=" + opts.StorageDriver,
		// Filled in below, once there is a spec to digest.
		SpecLabel + "=",
	}
	if opts.Workspace != "" {
		labels = append(labels, WorkspaceLabel+"="+opts.Workspace)
	}

	spec := Spec{
		Name:       ContainerName(account),
		Image:      image,
		Entrypoint: Entrypoint,
		Privileged: true,
		Labels:     labels,
		Mounts: append([]elevate.Mount{
			{Type: "bind", Source: SocketDir + "/" + account, Destination: SocketMount},
			{Type: "volume", Name: VolumeName(account), Destination: "/var/lib/docker"},
		}, opts.Mounts...),
		// exec because runc executes from the exec-root, and 0755 rather
		// than docker's tmpfs default of 1777.
		Tmpfs: []string{ExecRoot + ":rw,exec,mode=755"},
		// Empty, not unset. It no longer decides anything about the daemon,
		// since naming `dockerd` above means the block that reads it never
		// runs. What still reads it is dind's OTHER script,
		// docker-entrypoint.sh, which a `docker exec` into this container goes
		// through: with no DOCKER_HOST and no socket yet, a non-empty value
		// sends that client to tcp://docker:2376, a host this deployment does
		// not have.
		Env:     []string{"DOCKER_TLS_CERTDIR="},
		Command: command,
	}

	// Stamped last: the digest covers the spec, so it cannot be part of what
	// it digests.
	for i, l := range spec.Labels {
		if l == SpecLabel+"=" {
			spec.Labels[i] = SpecLabel + "=" + Fingerprint(spec)
		}
	}
	return spec, nil
}

// Args renders the spec as arguments to `docker run`, through the one renderer
// in elevate. Detached, and never removed: see the note on Spec.
func (s Spec) Args() []string {
	return elevate.RunSpec{
		Name:       s.Name,
		Image:      s.Image,
		Entrypoint: s.Entrypoint,
		Privileged: s.Privileged,
		Detach:     true,
		Labels:     s.Labels,
		Mounts:     s.Mounts,
		Env:        s.Env,
		Tmpfs:      s.Tmpfs,
		Command:    s.Command,
	}.Args()
}

// StorageDriverFrom picks a per-account storage driver out of the workspace's
// own dockerd arguments, as the default under WORKSPACE_DIND_STORAGE_DRIVER.
//
// A per-account daemon does NOT inherit its parent's flags, and the one flag
// where that matters is the graph driver: a deployment on Ceph- or NFS-backed
// storage sets fuse-overlayfs because overlay2 refuses such a filesystem, and
// the account's graph volume is on that same filesystem. Getting it wrong is
// silent, because dockerd falls back to vfs; Manager.warnIfSlowStorage is what
// that costs.
func StorageDriverFrom(dockerdArgs []string) string {
	for i, arg := range dockerdArgs {
		if v, ok := strings.CutPrefix(arg, "--storage-driver="); ok {
			return v
		}
		if arg == "--storage-driver" && i+1 < len(dockerdArgs) {
			return dockerdArgs[i+1]
		}
	}
	return ""
}

// Fingerprint digests the rendered arguments, minus its own label: image,
// entrypoint, flags, labels, mounts.
//
// A digest rather than a comparison of arguments means a new setting is
// noticed without anything having to be taught what settings exist. Adding one
// to Plan is enough.
func Fingerprint(spec Spec) string {
	h := sha256.New()
	for _, arg := range spec.Args() {
		if strings.HasPrefix(arg, SpecLabel+"=") {
			continue
		}
		_, _ = h.Write([]byte(arg))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// ParseMounts reads WORKSPACE_DIND_MOUNTS: a comma-separated list of
// source:destination or source:destination:ro.
//
//	/etc/docker/daemon.json:/etc/docker/daemon.json:ro,/etc/docker/certs.d:/etc/docker/certs.d:ro
//
// For what the daemon can only be given as files: a daemon.json naming an
// insecure or mirrored registry, the certificates for a registry with a
// private CA. A workspace mounts those into its own daemon and each account's
// needs the same, or a pull that works on the workspace fails inside every
// account.
//
// Both paths must be absolute. A relative source is not a path to docker, it is
// a VOLUME NAME, so `-v etc/docker:/etc/docker` silently creates an empty
// volume called "etc/docker" and the daemon reads no configuration at all.
//
// A destination the daemon already uses is refused rather than ordered after
// ours: docker rejects two mounts at one path, so the daemon would not start
// and the message would name the path rather than the setting that produced it.
func ParseMounts(spec string) ([]elevate.Mount, error) {
	var mounts []elevate.Mount

	for _, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		parts := strings.Split(entry, ":")
		if len(parts) < 2 || len(parts) > 3 {
			return nil, fmt.Errorf("daemons: %q is not source:destination[:ro]", entry)
		}
		source, destination := parts[0], parts[1]

		readOnly := false
		if len(parts) == 3 {
			if parts[2] != "ro" && parts[2] != "rw" {
				return nil, fmt.Errorf("daemons: %q has option %q, want ro or rw", entry, parts[2])
			}
			readOnly = parts[2] == "ro"
		}

		if !strings.HasPrefix(source, "/") || !strings.HasPrefix(destination, "/") {
			return nil, fmt.Errorf("daemons: %q needs absolute paths on both sides", entry)
		}
		if destination == SocketMount || destination == "/var/lib/docker" {
			return nil, fmt.Errorf("daemons: %q mounts over %s, which every daemon needs for itself",
				entry, destination)
		}

		mounts = append(mounts, elevate.Mount{
			Type:        "bind",
			Source:      source,
			Destination: destination,
			ReadOnly:    readOnly,
		})
	}
	return mounts, nil
}

// MissingSources reports the mounts whose source is not on this machine.
//
// docker CREATES a missing bind source, so `/typo:/lib/modules` would give the
// daemon an empty directory and surface inside somebody's container with
// nothing naming the setting. Separate from ParseMounts, which stays pure, and
// stat is injected for the same reason.
func MissingSources(mounts []elevate.Mount, stat func(string) error) []elevate.Mount {
	var out []elevate.Mount
	for _, m := range mounts {
		if stat(m.Source) != nil {
			out = append(out, m)
		}
	}
	return out
}

// DaemonPaths reports the paths a bind may name, because the workspace put them
// in the daemon's own filesystem (ADR 0041).
//
// Which side that is depends on the daemon resolving the bind, and is settled
// here so no use site has to ask: per-account, the DESTINATION, where the dind
// mount lands; shared, the SOURCE, since nothing is mounted and the workspace's
// own dockerd sees the path as it exists here. Identical for the usual
// /lib/modules:/lib/modules:ro.
func DaemonPaths(mounts []elevate.Mount, perAccount bool) []string {
	var out []string
	for _, m := range mounts {
		if perAccount {
			out = append(out, m.Destination)
		} else {
			out = append(out, m.Source)
		}
	}
	return out
}

// UnmountedRemaps are entries a shared daemon cannot honour: nothing is mounted
// there, so it has the source and nothing at the destination. Returned rather
// than logged, so the caller says it once with the setting named.
func UnmountedRemaps(mounts []elevate.Mount) []elevate.Mount {
	var out []elevate.Mount
	for _, m := range mounts {
		if m.Source != m.Destination {
			out = append(out, m)
		}
	}
	return out
}
