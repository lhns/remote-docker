package rewrite

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/lhns/remote-docker/pkg/workspace"

	"github.com/lhns/remote-docker/internal/logx"
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
// alone is not enough -- a user is perfectly entitled to name a volume
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
		if err := c.Remover.RemoveVolume(ctx, v.Name); err != nil {
			// A volume that cannot be removed is not fatal: it may have been
			// claimed by a container between the listing and now, which is a
			// race we lose harmlessly and retry next time.
			c.log().Warn("could not remove a volume", "volume", v.Name, "err", err)
			continue
		}
		removed++
		c.log().Info("removed an unused share volume", "volume", v.Name)
	}
	return removed, nil
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
	return true
}

// log is the collector's logger, or silence. A nil *slog.Logger panics on use.
func (c *Collector) log() *slog.Logger {
	if c.Log == nil {
		return logx.Discard()
	}
	return c.Log
}
