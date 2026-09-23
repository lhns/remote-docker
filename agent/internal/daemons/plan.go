// Package daemons gives each enrolled account its own Docker daemon, behind
// the same single SSH port (ADR 0019). SEPARATION, NOT ISOLATION: each runs
// privileged, so a determined account can still reach another's.
//
// The plan is pure, because the difference between a correct and a
// catastrophic invocation is one flag, and that belongs in a test.
package daemons

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/lhns/remote-docker/agent/internal/elevate"
)

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

// Entrypoint is what a per-account daemon runs: set, because the workspace's
// own image would otherwise run the agent, and dind's script rather than
// dockerd, because the script removes a stale /var/run/docker.pid that
// otherwise kills every RESTART with "process with PID 1 is still running".
// What it does with its first argument is on the command Plan builds.
const Entrypoint = "dockerd-entrypoint.sh"

// SocketDir is where the agent keeps one socket directory per account, and
// socketMount is where that directory appears inside the account's daemon: not
// /var/run, where it would hide containerd's own sockets.
const (
	SocketDir   = "/run/rd"
	socketMount = "/rd-sock"
	socketName  = "docker.sock"
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

// Labels identify a container as a daemon we manage, and whose. The workspace
// label is the persisted workspace id, never a container id, which changes on
// every redeploy and would orphan every account's daemon.
const (
	ManagedLabel   = "remote-docker.daemon"
	AccountLabel   = "remote-docker.account"
	WorkspaceLabel = "remote-docker.workspace"

	// specLabel and storageLabel record what a daemon was CREATED with: the
	// spec digested, and the graph driver on its own. An existing daemon is
	// started, never re-run, so without them a changed setting would reach
	// nobody who already has one. See Manager.reconcile.
	specLabel    = "remote-docker.spec"
	storageLabel = "remote-docker.storage-driver"
)

// Spec describes one account's daemon.
type Spec struct {
	Name       string
	Image      string
	Entrypoint string
	Privileged bool

	// No --rm, which would delete everything the account holds when the
	// daemon stops, and no restart policy, which would make the parent a
	// second supervisor (ADR 0019). plan_test.go pins both absent.

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

// ContainerName is what one account's daemon is called, derived so it can be
// found again after an agent restart.
func ContainerName(account string) string {
	return "rd-dind-" + account
}

// VolumeName is where one account's /var/lib/docker lives: a named volume on
// the workspace's daemon, on a real filesystem, outliving the daemon, the agent
// and a redeploy. Never collected automatically.
func VolumeName(account string) string {
	return "rd-dind-" + account + "-lib"
}

// SocketPathFor is where the agent dials one account's daemon.
func SocketPathFor(account string) string {
	return SocketDir + "/" + account + "/" + socketName
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
	// TCP: without it dind's entrypoint adds tcp://0.0.0.0:2375, an
	// unauthenticated Docker API reachable by every container the account runs
	// (CLAUDE.md, "The command handed to dockerd-entrypoint.sh NAMES dockerd").
	// (docker-library/docker `dockerd-entrypoint.sh`, read 2026-09-08; re-check
	// with `curl -s https://raw.githubusercontent.com/docker-library/docker/master/dockerd-entrypoint.sh`.)
	//
	// Two listeners: the one the agent dials, and the conventional path.
	command := []string{
		"dockerd",
		"-H", "unix://" + socketMount + "/" + socketName,
		"-H", "unix:///var/run/docker.sock",
	}
	if opts.StorageDriver != "" {
		command = append(command, "--storage-driver", opts.StorageDriver)
	}

	labels := []string{
		ManagedLabel + "=1",
		AccountLabel + "=" + account,
		storageLabel + "=" + opts.StorageDriver,
		// Filled in below, once there is a spec to digest.
		specLabel + "=",
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
			{Type: "bind", Source: SocketDir + "/" + account, Destination: socketMount},
			{Type: "volume", Name: VolumeName(account), Destination: "/var/lib/docker"},
		}, opts.Mounts...),
		// exec because runc executes from the exec-root, and 0755 rather
		// than docker's tmpfs default of 1777.
		Tmpfs: []string{ExecRoot + ":rw,exec,mode=755"},
		// Empty, not unset: dind's docker-entrypoint.sh, which a `docker
		// exec` goes through, sends a client with no DOCKER_HOST to
		// tcp://docker:2376 when it is set.
		Env:     []string{"DOCKER_TLS_CERTDIR="},
		Command: command,
	}

	// Stamped last: the digest covers the spec, so it cannot be part of what
	// it digests.
	for i, l := range spec.Labels {
		if l == specLabel+"=" {
			spec.Labels[i] = specLabel + "=" + Fingerprint(spec)
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
// own dockerd arguments, as the default under WORKSPACE_DIND_STORAGE_DRIVER. A
// per-account daemon inherits no flags, and a graph volume on Ceph or NFS
// needs the parent's fuse-overlayfs, or dockerd silently falls back to vfs
// (Manager.warnIfSlowStorage).
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

// Fingerprint digests the rendered arguments, minus its own label, so a setting
// added to Plan is noticed without anything being taught it exists.
func Fingerprint(spec Spec) string {
	h := sha256.New()
	for _, arg := range spec.Args() {
		if strings.HasPrefix(arg, specLabel+"=") {
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
// For what a daemon can only be given as files: a daemon.json, a registry's CA.
//
// Both paths absolute: a relative source is a VOLUME NAME to docker, silently
// created empty. A destination the daemon already uses is refused here, where
// the setting can be named, rather than by docker at start.
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
		if destination == socketMount || destination == "/var/lib/docker" {
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

// MissingSources reports the mounts whose source is not on this machine:
// docker CREATES a missing bind source, so a typo would be an empty directory
// with nothing naming the setting.
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
// in the daemon's own filesystem (ADR 0041): per-account the DESTINATION,
// where the dind mount lands; shared the SOURCE, since nothing is mounted.
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
