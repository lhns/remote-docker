package ephemeral

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/lhns/remote-docker/agent/internal/daemons"
	"github.com/lhns/remote-docker/agent/internal/dockercli"
	"github.com/lhns/remote-docker/agent/internal/metrics"
	"github.com/lhns/remote-docker/agent/internal/unions"
	"github.com/lhns/remote-docker/core/logx"
	"github.com/lhns/remote-docker/core/workspace"
)

// cleanupTimeout bounds one run's cleanup, which may boot a daemon, so one
// hung docker command cannot stop every later sweep.
const cleanupTimeout = 5 * time.Minute

// Docker is what cleanup asks of a daemon; dockercli.RunObjects.
type Docker interface {
	Containers(ctx context.Context, host, account, client string) ([]dockercli.Container, error)
	RemoveContainers(ctx context.Context, host string, ids []string) error
	Networks(ctx context.Context, host, client string, projects map[string]bool) ([]string, error)
	RemoveNetwork(ctx context.Context, host, name string) error
	Volumes(ctx context.Context, host, client string) ([]dockercli.Volume, error)
	VolumesInUse(ctx context.Context, host string) (map[string]bool, error)
	RemoveVolume(ctx context.Context, host, name string) error
}

// Unions is what cleanup asks of the union mounts; *unions.Manager.
type Unions interface {
	Release(ctx context.Context, account, client string)
	MountedCaches(account, client string, d unions.Daemon) ([]string, error)
}

// Cleaner removes what an expired run left on the workspace, and only what
// carries the run's client id. Anything it cannot remove, or cannot tell is
// unused, is kept and reported, so the run and its port wait for the next
// sweep (ADR 0050).
type Cleaner struct {
	Targets daemons.Targets
	Docker  Docker
	Unions  Unions

	// Containers also removes the run's containers and its compose networks
	// (WORKSPACE_EPHEMERAL_CLEANUP_CONTAINERS).
	Containers bool

	// Removed counts what cleanup removed, by kind; Kept what it kept, by kind
	// and reason. Nil counts nothing.
	Removed *metrics.Counter
	Kept    *metrics.Counter

	Log *slog.Logger
}

// The kinds of object cleanup removes, and why it keeps one, as the Removed
// and Kept counters label them.
const (
	KindContainer = "container"
	KindNetwork   = "network"
	KindVolume    = "volume"

	KeptInUse   = "in_use"
	KeptMounted = "mounted"
	KeptError   = "error"
)

// Clean is Registry.Cleanup: containers and networks, unions, volumes, in that
// order. The registry frees the port after it.
func (c *Cleaner) Clean(ctx context.Context, account, client string) error {
	if client == "" {
		return fmt.Errorf("ephemeral: a run of %s names no client", account)
	}
	log := logx.Or(c.Log).With("account", account, "client", client)
	ctx, cancel := context.WithTimeout(ctx, cleanupTimeout)
	defer cancel()

	// Ensure: a stopped daemon still holds the run's volumes.
	target, err := c.Targets.Ensure(ctx, account)
	if err != nil {
		return fmt.Errorf("cannot reach the daemon: %w", err)
	}
	host := target.Host

	kept := 0
	if c.Containers {
		cs, err := c.Docker.Containers(ctx, host, account, client)
		if err != nil {
			return err
		}
		// Read before the containers go: they are what ties a network to
		// this account.
		var ids []string
		projects := map[string]bool{}
		for _, ct := range cs {
			ids = append(ids, ct.ID)
			if ct.Project != "" {
				projects[ct.Project] = true
			}
		}
		if len(ids) > 0 {
			if err := c.Docker.RemoveContainers(ctx, host, ids); err != nil {
				c.Kept.Add(float64(len(ids)), KindContainer, KeptError)
				return err
			}
			c.Removed.Add(float64(len(ids)), KindContainer)
			log.Info("removed a run's containers", "count", len(ids))
		}

		nets, err := c.Docker.Networks(ctx, host, client, projects)
		if err != nil {
			return err
		}
		for _, n := range nets {
			if err := c.Docker.RemoveNetwork(ctx, host, n); err != nil {
				kept++
				c.Kept.Inc(KindNetwork, KeptError)
				log.Warn("keeping a run's network", "network", n, "why", err)
				continue
			}
			c.Removed.Inc(KindNetwork)
			log.Info("removed a run's network", "network", n)
		}
	}

	c.Unions.Release(ctx, account, client)

	vols, err := c.Docker.Volumes(ctx, host, client)
	if err != nil {
		return err
	}
	inUse, err := c.Docker.VolumesInUse(ctx, host)
	if err != nil {
		return err
	}
	caches, err := c.Unions.MountedCaches(account, client, unions.Daemon{Host: host, PID: target.PID})
	if err != nil {
		return err
	}
	mounted := map[string]bool{}
	for _, name := range caches {
		mounted[name] = true
	}

	for _, v := range vols {
		if !runOwns(v, account, client) {
			continue
		}
		why, reason := "", ""
		switch {
		case inUse[v.Name]:
			why, reason = "a container names it", KeptInUse
		case mounted[v.Name]:
			why, reason = "a union is mounted on it", KeptMounted
		}
		if why == "" {
			if err := c.Docker.RemoveVolume(ctx, host, v.Name); err != nil {
				why, reason = err.Error(), KeptError
			}
		}
		if why != "" {
			kept++
			c.Kept.Inc(KindVolume, reason)
			log.Info("keeping a run's volume", "volume", v.Name, "why", why)
			continue
		}
		c.Removed.Inc(KindVolume)
		log.Info("removed a run's volume", "volume", v.Name)
	}

	if kept > 0 {
		return fmt.Errorf("kept %d object(s) of the run", kept)
	}
	return nil
}

// runOwns reports whether a volume is this run's to remove: our prefix, our
// managed label, and this account and client. A user may name a volume rd-x.
func runOwns(v dockercli.Volume, account, client string) bool {
	return workspace.IsManagedVolume(v.Name) &&
		v.Labels[workspace.ManagedLabel] == workspace.ManagedShare &&
		v.Labels[workspace.OwnerLabel] == account &&
		v.Labels[workspace.ClientLabel] == client
}
