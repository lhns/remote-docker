// Package daemons gives each enrolled account its own Docker daemon.
//
// One workspace container ran one dockerd and every account connected to it
// (ADR 0012), so every user could list, inspect, exec into, stop and remove
// every other user's containers. This launches a dind per account instead,
// behind the same single SSH port. See docs/adr/0019.
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
// A LAST RESORT, not a good default, and the difference matters: stock
// docker:dind does not carry fuse-overlayfs. A workspace whose data is on Ceph
// or NFS runs its own dockerd with --storage-driver=fuse-overlayfs, which is
// why the image built here installs it, and a per-account daemon inheriting
// that driver on this image dies at startup with
//
//	exec: "fuse-overlayfs": executable file not found in $PATH
//	failed to start daemon: error initializing graphdriver: driver not supported
//
// in a restart loop. The right image is the workspace's OWN, which is this one
// plus exactly the tooling this project decided it needs; see ImageEnv.
const DefaultImage = "docker:28-dind"

// Entrypoint is what a per-account daemon runs.
//
// Set explicitly because the image is the workspace's own, whose entrypoint is
// the agent. Left alone, the daemon container would run `remote-dockerd`
// handed dockerd's flags.
//
// It is dind's OWN entrypoint script, not `dockerd`, and that distinction cost
// a CI run. The script does setup that dockerd does not do for itself, and the
// piece that matters is removing a stale /var/run/docker.pid. Without it the
// FIRST start works and every RESTART dies with
//
//	failed to start daemon, ensure docker is not running or delete
//	/var/run/docker.pid: process with PID 1 is still running
//
// in a loop, so a daemon looks fine until the workspace is restarted, which
// is exactly when nobody is watching. The script is present in both candidate
// images because the workspace's own is built FROM docker:dind.
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

	// SpecLabel records a digest of everything a daemon was created with:
	// image, entrypoint, flags, mounts, the lot.
	//
	// It exists because a daemon that already exists is STARTED, never re-run,
	// so its command line is fixed for life. Without a record of what it was
	// created from, changing the workspace's configuration silently applies to
	// nobody who already has a daemon, which, on any workspace that has been
	// used, is everybody.
	SpecLabel = "remote-docker.spec"

	// StorageLabel records the graph driver a daemon was CREATED with.
	//
	// A daemon that already exists is started, never re-run, which is what
	// keeps an account's containers and images across a redeploy. Its command
	// line is fixed for life, and without this label there is no way to
	// notice that the workspace's configuration has since changed and the
	// running daemon is not what the current settings would produce.
	//
	// It is a label rather than a comparison of command lines because the
	// question worth asking is "what was intended", not "what was typed".
	StorageLabel = "remote-docker.storage-driver"
)

// Spec describes one account's daemon.
type Spec struct {
	Name       string
	Image      string
	Entrypoint string
	Privileged bool

	// Remove is always false and the field is here to say so out loud. A
	// user's daemon holds their running containers, their images and their
	// volumes; `--rm` on it would delete all of that the moment it stopped.
	Remove bool

	// No restart policy, and this is where somebody will look for one. It
	// carried `unless-stopped` until ADR 0036, which made the parent dockerd a
	// second supervisor beside the agent: a daemon that would not start
	// crash-looped with nothing of ours watching. Ensure starts one when its
	// account connects, and that is the whole lifecycle.

	Labels []string
	Mounts []elevate.Mount
	Env    []string

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
	// has.
	//
	// For configuration the daemon reads from disk, which is the only way to
	// give it some things at all: /etc/docker/daemon.json for an insecure or
	// mirrored registry, /etc/docker/certs.d for a registry with a private CA.
	// A workspace mounts those into its own daemon and each account's needs the
	// same, or a pull that works on the workspace fails inside every account.
	//
	// Parsed from WORKSPACE_DIND_MOUNTS by ParseMounts.
	Mounts []elevate.Mount

	// StorageDriver is passed to the per-user dockerd. A deployment whose
	// graph volume is Ceph-backed sets fuse-overlayfs, and that has to reach
	// each account's daemon explicitly, because it is not inherited.
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

// HostFor is that same socket as a DOCKER_HOST value. It was written out as
// "unix://" + SocketPathFor(account) at three call sites.
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

	// Flags only: dockerd itself is the entrypoint. Two listeners, the one the
	// agent dials and the conventional path, so anything running inside the
	// daemon's own container still works.
	command := []string{
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
		Remove:     false,
		Labels:     labels,
		Mounts: append([]elevate.Mount{
			{Type: "bind", Source: SocketDir + "/" + account, Destination: SocketMount},
			{Type: "volume", Name: VolumeName(account), Destination: "/var/lib/docker"},
		}, opts.Mounts...),
		// TLS off: the only thing that can reach this daemon is the agent,
		// over a unix socket in a directory only the agent and the account can
		// enter. Certificates would secure a network path that does not exist.
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

// Args renders the spec as arguments to `docker run`.
func (s Spec) Args() []string {
	args := []string{"run", "-d"}
	if s.Remove {
		args = append(args, "--rm")
	}
	if s.Privileged {
		args = append(args, "--privileged")
	}
	if s.Entrypoint != "" {
		args = append(args, "--entrypoint", s.Entrypoint)
	}
	if s.Name != "" {
		args = append(args, "--name", s.Name)
	}
	for _, l := range s.Labels {
		args = append(args, "--label", l)
	}
	for _, e := range s.Env {
		args = append(args, "-e", e)
	}
	for _, m := range s.Mounts {
		args = append(args, "-v", m.Arg())
	}
	args = append(args, s.Image)
	return append(args, s.Command...)
}

// StorageDriverFrom picks a per-account storage driver out of the workspace's
// own dockerd arguments.
//
// A per-account daemon does NOT inherit its parent's flags, and the one flag
// where that matters is the graph driver. A deployment whose data directory is
// on Ceph- or NFS-backed storage sets --storage-driver=fuse-overlayfs for the
// workspace's dockerd because overlay2 refuses such a filesystem outright --
// and the per-account daemon's storage is a volume ON that same filesystem, so
// it needs the same answer.
//
// Getting this wrong is silent and expensive: dockerd falls back to VFS, which
// has no copy-on-write and copies the entire image on every container create.
// Nothing fails, so nothing says why -- `docker ps` stays instant while
// `docker create debian` takes 90 to 113 seconds.
//
// An explicit WORKSPACE_DIND_STORAGE_DRIVER still wins; this is only the
// default, and the default should be "what the workspace itself decided".
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

// Fingerprint digests everything about a spec that a running daemon cannot
// change without being recreated.
//
// The rendered arguments, hashed: image, entrypoint, flags, labels, mounts.
// Comparing the digest rather than the arguments means a new setting is
// noticed without anything having to be taught what settings exist. Adding
// one to Plan is enough.
//
// Its own label is excluded, since it is being computed.
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
