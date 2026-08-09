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
	"errors"
	"fmt"
	"strings"

	"github.com/lhns/remote-docker/internal/server/elevate"
)

// ErrUnsupported is returned by the parts that need a unix account database.
// The agent is Linux-only; this exists so the module still builds on the
// development machine.
var ErrUnsupported = errors.New("daemons: per-account daemons are Linux-only")

// DefaultImage is the dind image each account's daemon runs.
const DefaultImage = "docker:28-dind"

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
// `docker compose up -d` -- leaving the daemons running, unreferenced, holding
// their volumes and their users' containers.
const (
	ManagedLabel   = "remote-docker.daemon"
	AccountLabel   = "remote-docker.account"
	WorkspaceLabel = "remote-docker.workspace"

	// StorageLabel records the graph driver a daemon was CREATED with.
	//
	// A daemon that already exists is started, never re-run -- that is what
	// keeps an account's containers and images across a redeploy -- so its
	// command line is fixed for life. Without this label there is no way to
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
	Privileged bool

	// Remove is always false and the field is here to say so out loud. A
	// user's daemon holds their running containers, their images and their
	// volumes; `--rm` on it would delete all of that the moment it stopped.
	Remove bool

	// Restart is the policy the PARENT daemon applies to this one.
	//
	// Set, and it earns it: when the workspace is restarted, the parent
	// dockerd restarts only containers that asked to be. Without a policy
	// every account's daemon stays down until that account next connects --
	// so a detached container survives its author's disconnect, as intended,
	// but not the workspace being restarted, which is the moment it most
	// needs to.
	//
	// `unless-stopped` rather than `always`: an operator who deliberately
	// stops somebody's daemon should find it still stopped afterwards.
	Restart string

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

	// StorageDriver is passed to the per-user dockerd. A deployment whose
	// graph volume is Ceph-backed sets fuse-overlayfs, and that has to reach
	// each account's daemon explicitly -- it is not inherited.
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
// rather than on an overlay -- overlay2 on overlay2 never arises -- and so it
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

	// Two listeners: the one the agent dials, and the conventional path so
	// that anything running inside the daemon's own container still works.
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
	}
	if opts.Workspace != "" {
		labels = append(labels, WorkspaceLabel+"="+opts.Workspace)
	}

	return Spec{
		Name:       ContainerName(account),
		Image:      image,
		Privileged: true,
		Remove:     false,
		Restart:    "unless-stopped",
		Labels:     labels,
		Mounts: []elevate.Mount{
			{Type: "bind", Source: SocketDir + "/" + account, Destination: SocketMount},
			{Type: "volume", Name: VolumeName(account), Destination: "/var/lib/docker"},
		},
		// TLS off: the only thing that can reach this daemon is the agent,
		// over a unix socket in a directory only the agent and the account can
		// enter. Certificates would secure a network path that does not exist.
		Env:     []string{"DOCKER_TLS_CERTDIR="},
		Command: command,
	}, nil
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
	if s.Restart != "" {
		args = append(args, "--restart", s.Restart)
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
// It was not inherited, and the failure was silent and expensive: dockerd falls
// back to VFS, which has no copy-on-write and copies the entire image on every
// container create. Everything kept working and `docker create debian` took 90
// to 113 seconds while `docker ps` stayed instant. Nothing said why, because
// nothing had failed.
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
