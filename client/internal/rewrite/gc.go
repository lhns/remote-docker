package rewrite

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/lhns/remote-docker/core/logx"
	"github.com/lhns/remote-docker/core/workspace"
)

// Volume is a volume as the daemon reports it.
type Volume struct {
	Name   string
	Labels map[string]string
}

// VolumeStore is what garbage collection asks of the daemon.
type VolumeStore interface {
	ListVolumes(ctx context.Context) ([]Volume, error)
	RemoveVolume(ctx context.Context, name string) error
	VolumesInUse(ctx context.Context) (map[string]bool, error)
}

// Collector removes NFS-backed volumes this client created once nothing is
// using them; per-bind volumes otherwise accumulate (ADR 0006).
type Collector struct {
	Volumes VolumeStore

	// Caches names the cache volumes the workspace has a union on. Nil means
	// it cannot be asked, and no cache volume is collected.
	Caches func(ctx context.Context) (map[string]bool, error)

	// Owner limits collection to this account's volumes.
	Owner string

	// Client limits collection to this machine's volumes, since one account's
	// machines share the owner label (ADR 0029). Empty collects regardless.
	Client string

	// Orphans also collects volumes naming no machine at all, and nothing more.
	Orphans bool

	// Guard is shared with the Rewriter, so a volume just created for a
	// `docker run` is not deleted before a container references it.
	Guard *Guard

	Log *slog.Logger
}

// Collect removes unused managed volumes and reports how many went. A volume
// must carry both our prefix AND our label: a user may name one "rd-backups".
func (c *Collector) Collect(ctx context.Context) (int, error) {
	volumes, err := c.Volumes.ListVolumes(ctx)
	if err != nil {
		return 0, fmt.Errorf("rewrite: listing volumes: %w", err)
	}

	inUse, err := c.Volumes.VolumesInUse(ctx)
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
		if held, why := c.cacheHeld(ctx, v.Name); held {
			c.log().Debug("keeping a cache volume", "volume", v.Name, "why", why)
			continue
		}
		gone, err := c.remove(ctx, v.Name)
		if err != nil {
			// Likely claimed since the listing; retried next time.
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

// cacheHeld reports whether a cache volume must be kept, and why. The daemon
// always calls one unused (a union is bound by path), so the workspace is
// asked, and cannot-ask means keep (ADR 0044).
func (c *Collector) cacheHeld(ctx context.Context, name string) (bool, string) {
	if !workspace.IsCacheVolume(name) {
		return false, ""
	}
	if c.Caches == nil {
		return true, "no way to ask the workspace which caches are mounted"
	}
	mounted, err := c.Caches(ctx)
	if err != nil {
		return true, "the workspace could not say which caches are mounted"
	}
	if mounted[name] {
		return true, "the workspace has a union on it"
	}
	return false, ""
}

// remove deletes a volume unless this session exports the directory behind
// it, deciding and deleting under the guard so a concurrent rewrite either
// spares it or recreates it.
func (c *Collector) remove(ctx context.Context, name string) (bool, error) {
	defer c.Guard.hold()()

	if c.Guard.exported(name) {
		return false, nil
	}
	if err := c.Volumes.RemoveVolume(ctx, name); err != nil {
		return false, err
	}
	return true, nil
}

// ours reports whether a volume is one we created and may delete.
func (c *Collector) ours(v Volume) bool {
	if !workspace.IsManagedVolume(v.Name) {
		return false
	}
	if v.Labels[workspace.ManagedLabel] != workspace.ManagedShare {
		return false
	}
	if c.Owner != "" && v.Labels[workspace.OwnerLabel] != c.Owner {
		return false
	}
	// No client label is not "mine": only `remote gc --orphans` takes those.
	if c.Client != "" {
		client := v.Labels[workspace.ClientLabel]
		unnamed := client == "" && c.Orphans
		if client != c.Client && !unnamed {
			return false
		}
	}
	return true
}

func (c *Collector) log() *slog.Logger {
	return logx.Or(c.Log)
}
