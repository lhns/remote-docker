package ephemeral

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/lhns/remote-docker/agent/internal/daemons"
	"github.com/lhns/remote-docker/agent/internal/dockercli"
	"github.com/lhns/remote-docker/agent/internal/unions"
	"github.com/lhns/remote-docker/core/logx"
	"github.com/lhns/remote-docker/core/workspace"
)

// cleanupTimeout bounds one run's cleanup, which may boot a daemon, so one
// hung docker command cannot stop every later sweep.
const cleanupTimeout = 5 * time.Minute

// Docker is what cleanup asks of a daemon; dockercli.RunObjects.
type Docker interface {
	Containers(ctx context.Context, host, account, client string) ([]string, error)
	RemoveContainers(ctx context.Context, host string, ids []string) error
	Networks(ctx context.Context, host, client string) ([]string, error)
	RemoveNetwork(ctx context.Context, host, name string) error
	Volumes(ctx context.Context, host, client string) ([]dockercli.Volume, error)
	VolumesInUse(ctx context.Context, host string) (map[string]bool, error)
	RemoveVolume(ctx context.Context, host, name string) error
}

// Unions is what cleanup asks of the union mounts; *unions.Manager.
type Unions interface {
	Release(ctx context.Context, account, client string)
	MountedCaches(account, client string, d unions.Daemon) []string
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

	Log *slog.Logger
}

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
		ids, err := c.Docker.Containers(ctx, host, account, client)
		if err != nil {
			return err
		}
		if len(ids) > 0 {
			if err := c.Docker.RemoveContainers(ctx, host, ids); err != nil {
				return err
			}
			log.Info("removed a run's containers", "count", len(ids))
		}

		nets, err := c.Docker.Networks(ctx, host, client)
		if err != nil {
			return err
		}
		for _, n := range nets {
			if err := c.Docker.RemoveNetwork(ctx, host, n); err != nil {
				kept++
				log.Warn("keeping a run's network", "network", n, "why", err)
				continue
			}
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
	mounted := map[string]bool{}
	for _, name := range c.Unions.MountedCaches(account, client, unions.Daemon{Host: host, PID: target.PID}) {
		mounted[name] = true
	}

	for _, v := range vols {
		if !runOwns(v, account, client) {
			continue
		}
		why := ""
		switch {
		case inUse[v.Name]:
			why = "a container names it"
		case mounted[v.Name]:
			why = "a union is mounted on it"
		}
		if why == "" {
			if err := c.Docker.RemoveVolume(ctx, host, v.Name); err != nil {
				why = err.Error()
			}
		}
		if why != "" {
			kept++
			log.Info("keeping a run's volume", "volume", v.Name, "why", why)
			continue
		}
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
