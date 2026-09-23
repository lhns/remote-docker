// What the session says about itself, and when it may be let go: the
// connection (idle sweep) or the process (status, expiry). Both ask
// hasLiveDependents.

package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/lhns/remote-docker/client/internal/proxy"
	"github.com/lhns/remote-docker/client/internal/rewrite"
	"github.com/lhns/remote-docker/core/workspace"
)

// sweepIdle releases the connection when nothing needs it.
func (s *Session) sweepIdle() {
	interval := max(s.opts.IdleTimeout/2, time.Second)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.gate.sweep(s.ctx)
		}
	}
}

// hasLiveDependents reports whether a running container of ours (published
// ports) or one holding our volumes (NFS mounts) still needs the connection.
// Streams are counted by their gate lease, not here. Volumes are matched
// against ours, never the `rd-` prefix, which on a shared daemon (ADR 0012)
// matches other accounts' and would pin the connection forever.
func (s *Session) hasLiveDependents(ctx context.Context, live *liveConn) (bool, error) {
	containers, err := live.api.ListContainers(ctx)
	if err != nil {
		return false, err
	}
	ours := s.ourVolumes()
	for _, c := range containers {
		// Scoped to this machine too, or another machine's containers would
		// block this one's idle release (ADR 0029). No client label predates
		// machine names and counts.
		if c.Labels[workspace.OwnerLabel] == live.info.User {
			if client := c.Labels[workspace.ClientLabel]; client == "" || client == s.clientID {
				return true, nil
			}
		}
		for _, m := range c.Mounts {
			if m.Type == "volume" && ours[m.Name] {
				return true, nil
			}
		}
	}
	return false, nil
}

// ourVolumes names the volumes backing this session's shares, derived from
// the registry (ADR 0007).
func (s *Session) ourVolumes() map[string]bool {
	shares := s.registry.Shares()
	out := make(map[string]bool, len(shares))
	for _, share := range shares {
		if name, err := workspace.VolumeNameForExport(s.clientID, share.ExportPath); err == nil {
			out[name] = true
		}
	}
	return out
}

func (live *liveConn) close() {
	if live.cancel != nil {
		live.cancel()
	}
	if live.notify != nil {
		_ = live.notify.Close()
	}
	if live.ports != nil {
		_ = live.ports.Close()
	}
	if live.nfsTunnel != nil {
		_ = live.nfsTunnel.Close()
	}
	_ = live.ssh.Close()
	live.wg.Wait()

	// Last: the machine may shut down once nothing holds it.
	if live.machine != nil {
		_ = live.machine.Close()
	}
}

// CollectOptions widens what a collection is allowed to remove.
type CollectOptions struct {
	// Orphans also removes unused share volumes that name no machine. Opt-in:
	// another machine on an older build may still use one.
	Orphans bool
}

// Collect removes share volumes this account is no longer using.
func (s *Session) Collect(ctx context.Context, opts CollectOptions) (int, error) {
	live, done, err := s.acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer done()

	collector := s.collector(live)
	// Never clear Client: that would collect other machines' volumes.
	collector.Orphans = opts.Orphans

	n, err := collector.Collect(ctx)
	if err == nil {
		s.pruneShareRecord(ctx, live)
	}
	return n, err
}

// pruneShareRecord drops record entries whose volume is gone. Best effort.
func (s *Session) pruneShareRecord(ctx context.Context, live *liveConn) {
	if s.shares == nil {
		return
	}

	volumes, err := live.api.ListVolumes(ctx)
	if err != nil {
		return
	}
	keep := make(map[string]bool, len(volumes))
	for _, v := range volumes {
		client, share, ok := workspace.ParseVolumeName(v.Name)
		if !ok || (client != "" && client != s.clientID) {
			continue
		}
		if share != "cwd" {
			keep[workspace.ExportPathForID(share)] = true
		}
	}
	s.shares.forget(keep)
}

func (s *Session) collector(live *liveConn) *rewrite.Collector {
	return &rewrite.Collector{
		Volumes: live.api,
		Owner:   live.info.User,
		Client:  s.clientID,
		Guard:   live.guard,
		Caches:  s.mountedCaches,
		Log:     s.opts.Log,
	}
}

// mountedCaches asks the workspace which cache volumes it has a union on:
// the daemon never calls one in use, and an earlier session's share is not
// in this registry (ADR 0044).
func (s *Session) mountedCaches(ctx context.Context) (map[string]bool, error) {
	live := s.liveCache()
	if live == nil {
		return nil, errors.New("session: no cache channel to ask which caches are mounted")
	}
	names, err := live.Mounted(ctx)
	if err != nil {
		return nil, err
	}

	mounted := make(map[string]bool, len(names))
	for _, n := range names {
		mounted[n] = true
	}
	return mounted, nil
}

// exportsVolume reports whether a volume backs a share this session exports.
// The daemon calls it in use only once a container names it, and it must
// survive collection before that.
func (s *Session) exportsVolume(volume string) bool {
	return s.ourVolumes()[volume]
}

// Status answers the control endpoint, satisfying proxy.Control. It never
// connects: asking must not change the answer.
func (s *Session) Status() any {
	// currentLive: a held connection that has died is not connected.
	live, connected := s.gate.currentLive()
	st := proxy.Status{
		Version:   s.opts.Version,
		PID:       os.Getpid(),
		Connected: connected,
		Since:     s.started.Format(time.RFC3339),
		Tracing:   proxy.Tracing(),
	}
	if drops, last := s.gate.dropped(); drops > 0 {
		st.Drops = drops
		st.LastDrop = last.Format(time.RFC3339)
	}
	if connected {
		st.Storage = live.info.Storage
	}
	st.Caches = s.cacheStatus()
	return st
}

// Idle reports whether this session could be ended without breaking anything,
// satisfying proxy.Control.
func (s *Session) Idle() any {
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()

	_, safe := s.IdleFor(ctx)
	return proxy.Idle{Safe: safe}
}

// Shutdown asks the session to stop, satisfying proxy.Control. It returns at
// once: the caller's control connection is what Close shuts.
func (s *Session) Shutdown() {
	go func() {
		s.stopOnce.Do(func() { close(s.stopped) })
	}()
}

// IdleFor reports how long this session has been idle and whether ending the
// process is safe. No connection held means the gate released it because
// nothing depended on it; a held one is asked, and cannot-tell means busy.
func (s *Session) IdleFor(ctx context.Context) (time.Duration, bool) {
	last, inUse := s.gate.lastUse()
	if inUse {
		return 0, false
	}
	// Never used: idle since the start, or an unused daemon never expires.
	if last.IsZero() {
		last = s.started
	}
	quiet := time.Since(last)

	// A dead connection is not asked whether it is busy: it cannot answer, and
	// `remote restart` would refuse the session that most needs it.
	live, connected := s.gate.currentLive()
	if !connected {
		return quiet, true
	}

	busy, err := s.hasLiveDependents(ctx, live)
	if err != nil || busy {
		return quiet, false
	}
	return quiet, true
}

// Stopped is closed when something has asked this session to stop.
func (s *Session) Stopped() <-chan struct{} { return s.stopped }

// Close tears the session down.
func (s *Session) Close() error {
	if s.watch != nil {
		_ = s.watch.Close()
	}
	// After the watcher, so no event can arm the timer again.
	if s.cache != nil {
		s.cache.Stop()
	}
	s.once.Do(func() {
		s.cancel()
		if s.listener != nil {
			_ = s.listener.Close()
		}
		s.gate.close()
		s.wg.Wait()
	})
	return nil
}

// humanBytes formats a byte count for reading at a glance.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for n/div >= unit && exp < 3 {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGT"[exp])
}

// cacheStatus is one line per delegated share, saying how much is cached.
func (s *Session) cacheStatus() []string {
	if s.cache == nil {
		return nil
	}

	var out []string
	for _, r := range s.cache.Reports() {
		local, stats := r.Local, r.Stats

		what := "filling"
		switch {
		case r.Err != nil:
			what = "stopped: " + r.Err.Error()
		case !r.Done:
		case stats.Complete():
			what = "cached"
		default:
			// Over budget or partly unreadable: the rest is read live.
			what = "cached in part; the rest is read live"
		}

		// "N of M" only once the walk has finished and M is known.
		if r.Done {
			line := fmt.Sprintf("%s: %d of %d files, %s of %s, %s sent, %s",
				local, r.Sent, stats.TotalFiles,
				humanBytes(stats.Bytes), humanBytes(stats.TotalBytes), humanBytes(r.Bytes), what)
			if stats.Excluded > 0 {
				line += fmt.Sprintf(" (%d excluded)", stats.Excluded)
			}
			out = append(out, line)
			continue
		}
		out = append(out, fmt.Sprintf("%s: %d files so far, %s sent, %s", local, r.Sent, humanBytes(r.Bytes), what))
	}
	sort.Strings(out)
	return out
}
