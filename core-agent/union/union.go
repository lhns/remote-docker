// Package union mounts a delegated share as a cache over the live export
// (ADR 0044).
//
// Three layers, all inside the mount namespace of the daemon that will serve
// the container:
//
//	lower   the client's NFS export, live and correct
//	upper   a local cache, fast, and where the container's writes land
//	merged  what the container binds
//
// A read the cache has costs local disk; a read it does not have falls through
// to the lower and is right. That is what lets the cache be filled in the
// background and be incomplete at every moment without ever being wrong.
//
// # Why a separate process does the work
//
// Two kernel facts force it, each stated on the code that meets it: setns
// refuses a caller sharing filesystem state, and a thread that unshares its own
// can never rejoin the runtime's pool (see enter, serve_linux.go); and libfuse
// cannot read /proc/self across a pid namespace (see Serve). So the agent
// re-executes itself with Command, and the child enters the namespace, prepares
// the layers and becomes fuse-overlayfs.
package union

import (
	"fmt"
	"path"
	"strings"

	"github.com/lhns/remote-docker/core/workspace"
)

// Command is the argument the agent re-executes itself with. It is not a
// user-facing verb: the agent runs it on itself, and a person running it by
// hand has no way to supply the namespace it needs.
const Command = "union-serve"

// Binary is what performs the union. Not built in: it is a mature
// implementation of exactly this, it is already in the workspace image for the
// Ceph storage driver, and the kernel's own overlay cannot be used here at all
// -- an overlay whose lower is NFS is readable only from the mount namespace
// that created it, so a container gets EOPNOTSUPP on every file it should have
// fallen through to (measured, test/union-probe.sh).
const Binary = "fuse-overlayfs"

// Root is where a share's mountpoints live inside the daemon's namespace.
//
// Under /run, which is a tmpfs: these are mountpoints and nothing else, and
// losing them when the daemon restarts is right, because the mounts are gone
// then too.
const Root = "/run/rd-union"

// Spec is one share's union, fully resolved. Everything in it is a path or a
// number the agent worked out; nothing here is taken from the client except
// through cache.Request.Validate.
type Spec struct {
	// PID is the daemon whose mount namespace the union belongs in. Zero means
	// this process's own, which is the shared-daemon mode (ADR 0012), where
	// the agent and the daemon are the same filesystem.
	PID int

	// Export is the share, "/cwd" or "/m/<id>". It names the mountpoints and
	// is what the client asked for.
	Export string

	// Port is the client's reverse-tunnel port for the NFS export, inside the
	// daemon's network namespace.
	Port int

	// CacheDir is the cache volume's data directory, as the daemon reports it.
	// It must be on a real filesystem: the kernel refuses a union upper on
	// overlayfs, and a dind's own root is overlayfs.
	CacheDir string

	// Read is the share's read mode, which decides the attribute cache on the
	// LOWER. A read the union's upper does not hold falls through to it, so
	// this is what such a read costs.
	Read workspace.Read
}

// id is the share's identifier, used to name its mountpoints.
func (s Spec) id() string {
	if s.Export == workspace.ExportCWD {
		return workspace.CWDShareID
	}
	return strings.TrimPrefix(s.Export, workspace.ExportMountPrefix)
}

// Lower is where the client's export is mounted.
func (s Spec) Lower() string { return path.Join(Root, s.id(), "lower") }

// Merged is the union itself, and what a container binds.
func (s Spec) Merged() string { return path.Join(Root, s.id(), "merged") }

// Upper is the cache layer, inside the cache volume.
func (s Spec) Upper() string { return path.Join(s.CacheDir, "upper") }

// Work is fuse-overlayfs's scratch directory, which must be on the same
// filesystem as the upper.
func (s Spec) Work() string { return path.Join(s.CacheDir, "work") }

// Dirs are every directory the child creates before mounting anything.
func (s Spec) Dirs() []string {
	return []string{s.Lower(), s.Merged(), s.Upper(), s.Work()}
}

// LowerMount is the source, filesystem type, per-filesystem data and mount
// FLAGS for the lower.
//
// The same options a share's volume would have been given, because it is the
// same mount: workspace.NFSVolumeOptions is asked rather than copied, so the
// two cannot drift. The lower carries the share's read mode: a read the upper
// does not hold falls through, and a direct lower under a cached share
// revalidates every file every second (ADR 0044).
//
// Split, because that option list is written for DOCKER, which separates the
// two before it calls mount(2) and we have to as well. `noatime` is a kernel
// mount flag rather than something the NFS client parses, so passing the list
// through whole makes the NFS parser reject the lot -- and it reports that as
// EINVAL, which surfaces as `invalid argument` against a mount whose options
// are, one at a time, all valid.
func (s Spec) LowerMount() (source, fstype, data string, flags []string) {
	opts := workspace.NFSVolumeOptions(s.Port, s.Export, s.Read)

	var kept []string
	for _, opt := range strings.Split(opts["o"], ",") {
		if mountFlags[opt] {
			flags = append(flags, opt)
			continue
		}
		kept = append(kept, opt)
	}
	return opts["device"], opts["type"], strings.Join(kept, ","), flags
}

// mountFlags are the option words the KERNEL takes as flags rather than
// handing to the filesystem.
//
// Only the ones NFSVolumeOptions can produce today plus the ones a share could
// plausibly gain, so a `ro` added there later is carried rather than passed to
// the NFS parser, which would refuse it and take the mount down with it. Turned
// into MS_ constants where that is possible, which is the Linux file.
var mountFlags = map[string]bool{
	"noatime": true, "atime": true, "relatime": true, "strictatime": true,
	"ro": true, "rw": true,
	"nosuid": true, "nodev": true, "noexec": true,
	"sync": true, "dirsync": true,
}

// Args are what fuse-overlayfs is run with.
//
// -f keeps it in the foreground. Without it the process daemonises and the
// agent's child exits immediately, which would look exactly like the union
// having died at once and would make supervision impossible.
func (s Spec) Args() []string {
	return []string{
		Binary,
		"-f",
		"-o", fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", s.Lower(), s.Upper(), s.Work()),
		s.Merged(),
	}
}

// Validate refuses a spec that could not describe a real share.
//
// The client's request is validated separately and first; this is about what
// the agent itself assembled, and it exists because every field below becomes
// a privileged mount inside somebody's daemon.
func (s Spec) Validate() error {
	if err := workspace.ValidExport(s.Export); err != nil {
		return fmt.Errorf("union: %w", err)
	}
	if s.Port < 1 || s.Port > workspace.MaxPort {
		return fmt.Errorf("union: %s has port %d, which is not one", s.Export, s.Port)
	}
	if !path.IsAbs(s.CacheDir) || s.CacheDir == "/" {
		return fmt.Errorf("union: %s has cache directory %q, which is not a path to a volume",
			s.Export, s.CacheDir)
	}
	if s.PID < 0 {
		return fmt.Errorf("union: %s names pid %d", s.Export, s.PID)
	}
	switch s.Read {
	case workspace.ReadDirect, workspace.ReadCached:
	default:
		return fmt.Errorf("union: %s has read mode %q, which is not one", s.Export, s.Read)
	}
	return nil
}
