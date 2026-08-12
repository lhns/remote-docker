package rewrite

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/lhns/remote-docker/core/workspace"

	"github.com/lhns/remote-docker/core/logx"
)

// Volume is a volume as the daemon reports it.
type Volume struct {
	Name   string
	Labels map[string]string
}

// VolumeLister and VolumeRemover are the daemon operations garbage collection
// needs, kept separate from VolumeEnsurer so a caller that only creates
// volumes need not implement removal.
type VolumeLister interface {
	ListVolumes(ctx context.Context) ([]Volume, error)
}

// VolumeRemover deletes a volume by name.
type VolumeRemover interface {
	RemoveVolume(ctx context.Context, name string) error
}

// InUse reports the volume names currently referenced by a container.
type InUse interface {
	VolumesInUse(ctx context.Context) (map[string]bool, error)
}

// Collector removes NFS-backed volumes this client created once nothing is
// using them.
//
// They accumulate otherwise: a volume is created per distinct bind source, per
// project, and they outlive the containers that referenced them. ADR 0006
// records this as a consequence of per-bind volumes, and it is one that has to
// be handled rather than only noted.
type Collector struct {
	Volumes VolumeLister
	Remover VolumeRemover
	InUse   InUse

	// Owner limits collection to this account's volumes.
	Owner string

	// Client limits collection to THIS MACHINE's volumes.
	//
	// The owner label is not enough once one account is used from two
	// machines: both label with the same account, so each machine's collector
	// would delete the other's volumes, and losing one is not a tidy failure.
	// The daemon recreates a missing named volume as an empty local one, so
	// the container comes up with an empty directory where the project should
	// be.
	//
	// Empty collects regardless, which is what a workspace only ever reached
	// from one machine wants.
	Client string

	// Orphans also collects volumes that name NO machine, which is what a
	// version before machines were named left behind, or what this machine
	// left when its key was replaced.
	//
	// It widens by exactly that and no further. Collecting everything with the
	// right account instead would take the OTHER machine's volumes, which is
	// the failure this whole scoping exists to prevent.
	Orphans bool

	// Guard is shared with the Rewriter. Without it this can delete a volume a
	// concurrent `docker run` created a moment ago and has not yet referenced
	// from a container.
	Guard *Guard

	Log *slog.Logger
}

// Collect removes unused managed volumes and reports how many went.
//
// Two independent checks have to pass before anything is deleted, because
// deleting a volume the user created would destroy their data:
//
//  1. the name carries our prefix, and
//  2. the volume carries our label
//
// A volume failing either is not ours, whatever it looks like. The prefix
// alone is not enough, because a user is entitled to name a volume
// "rd-backups".
func (c *Collector) Collect(ctx context.Context) (int, error) {
	volumes, err := c.Volumes.ListVolumes(ctx)
	if err != nil {
		return 0, fmt.Errorf("rewrite: listing volumes: %w", err)
	}

	inUse, err := c.InUse.VolumesInUse(ctx)
	if err != nil {
		return 0, fmt.Errorf("rewrite: finding volumes in use: %w", err)
	}

	removed := 0
	for _, v := range volumes {
		if !c.ours(v) {
			continue
		}
		if inUse[v.Name] {
			continue
		}
		gone, err := c.remove(ctx, v.Name)
		if err != nil {
			// A volume that cannot be removed is not fatal: it may have been
			// claimed by a container between the listing and now, which is a
			// race we lose harmlessly and retry next time.
			c.log().Warn("could not remove a volume", "volume", v.Name, "err", err)
			continue
		}
		if !gone {
			continue
		}
		removed++
		c.log().Info("removed an unused share volume", "volume", v.Name)
	}
	return removed, nil
}

// remove deletes a volume unless this session is exporting the directory
// behind it, and reports whether it went.
//
// The decision and the deletion happen under the guard, so a bind rewrite
// either registered its share first (and the volume is spared) or arrives
// after the removal and recreates it. The daemon's own answer cannot cover
// this: a volume exists for a moment before any container names it, and that
// moment is exactly when this runs.
func (c *Collector) remove(ctx context.Context, name string) (bool, error) {
	defer c.Guard.hold()()

	if c.Guard.exported(name) {
		return false, nil
	}
	if err := c.Remover.RemoveVolume(ctx, name); err != nil {
		return false, err
	}
	return true, nil
}

// ours reports whether a volume is one we created and may delete.
func (c *Collector) ours(v Volume) bool {
	if !workspace.IsManagedVolume(v.Name) {
		return false
	}
	if v.Labels[ManagedLabel] != "share" {
		return false
	}
	// On a shared daemon, another account's share volumes are not ours to
	// remove even though they carry the same prefix and label.
	if c.Owner != "" && v.Labels[OwnerLabel] != c.Owner {
		return false
	}
	// Nor are another MACHINE's, which carry this account's label as well.
	//
	// A volume with no client label at all was created before machines were
	// named. It is left alone here rather than treated as ours, because "no
	// label" is not "mine": an older session of the other machine may still be
	// using it. `remote gc --orphans` is how those go, deliberately by asking.
	if c.Client != "" {
		client := v.Labels[ClientLabel]
		unnamed := client == "" && c.Orphans
		if client != c.Client && !unnamed {
			return false
		}
	}
	return true
}

// log is the collector's logger, or silence. A nil *slog.Logger panics on use.
func (c *Collector) log() *slog.Logger {
	if c.Log == nil {
		return logx.Discard()
	}
	return c.Log
}
