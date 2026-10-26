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

// dependentsTimeout bounds asking the workspace what still depends on us. A
// variable so a test need not wait for it.
var dependentsTimeout = 10 * time.Second

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
	// Bounded here: every caller's context is never cancelled, so a daemon
	// that stops answering would stall the idle sweep, Standby and expiry.
	ctx, cancel := context.WithTimeout(ctx, dependentsTimeout)
	defer cancel()

	containers, err := live.api.ListContainers(ctx)
	if err != nil {
		return false, err
	}
	ours := s.ourVolumes(live.clientID)
	for _, c := range containers {
		// Scoped to this machine too, or another machine's containers would
		// block this one's idle release (ADR 0029). No client label predates
		// machine names and counts.
		if c.Labels[workspace.OwnerLabel] == live.info.User {
			if client := c.Labels[workspace.ClientLabel]; client == "" || client == live.clientID {
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
func (s *Session) ourVolumes(clientID string) map[string]bool {
	shares := s.registry.Shares()
	out := make(map[string]bool, len(shares))
	for _, share := range shares {
		if name, err := workspace.VolumeNameForExport(clientID, share.ExportPath); err == nil {
			out[name] = true
		}
	}
	return out
}

func (live *liveConn) close() {
	if live.cancel != nil {
		live.cancel()
	}
	live.notifyMu.Lock()
	live.shut = true
	notify := live.notify
	live.notifyMu.Unlock()
	if notify != nil {
		_ = notify.Close()
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
	names := make([]string, 0, len(volumes))
	for _, v := range volumes {
		names = append(names, v.Name)
	}
	var registered []string
	for _, share := range s.registry.Shares() {
		registered = append(registered, share.ExportPath)
	}
	s.shares.forget(recordsToKeep(names, live.clientID, registered))
}

// recordsToKeep is the exports whose record survives a prune: those this machine
// has a volume for, and those this session has registered. The second matters
// because a share is registered, and recorded, BEFORE its volume exists (a
// union is prepared first, which takes seconds), so a prune landing in that gap
// forgot the record of a share a container was about to bind, and a client
// restarted under that container could not restore it.
func recordsToKeep(volumeNames []string, clientID string, registered []string) map[string]bool {
	keep := make(map[string]bool, len(volumeNames)+len(registered))
	for _, name := range volumeNames {
		client, share, ok := workspace.ParseVolumeName(name)
		if !ok || (client != "" && client != clientID) {
			continue
		}
		keep[workspace.ExportPathForID(share)] = true
	}
	for _, export := range registered {
		keep[export] = true
	}
	return keep
}

func (s *Session) collector(live *liveConn) *rewrite.Collector {
	return &rewrite.Collector{
		Volumes: live.api,
		Owner:   live.info.User,
		Client:  live.clientID,
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

// exportsVolume reports whether a volume backs a share this session exports,
// or is the cache over one. The daemon calls it in use only once a container
// names it, and it must survive collection before that.
func (s *Session) exportsVolume(clientID, volume string) bool {
	for name := range s.ourVolumes(clientID) {
		if volume == name || volume == workspace.CacheVolumeName(name) {
			return true
		}
	}
	return false
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
		Settings:  s.opts.Config.SessionSettings(),
	}
	if drops, last := s.gate.dropped(); drops > 0 {
		st.Drops = drops
		st.LastDrop = last.Format(time.RFC3339)
	}
	if connected {
		st.Storage = live.info.Storage
	}
	st.Caches = s.cacheStatus()
	st.WriteBack = s.writeBackStatus()
	return st
}

// Idle reports whether this session could be ended without breaking anything,
// satisfying proxy.Control.
func (s *Session) Idle() any {
	_, safe := s.IdleFor(s.ctx)
	return proxy.Idle{Safe: safe}
}

// Run names this session's run, satisfying proxy.Control, for a one-off query
// on this machine to join (ADR 0050).
func (s *Session) Run() any { return proxy.Run{Run: s.runID} }

// Client names this session's client, satisfying proxy.Control. Unlike Status
// it connects: an ephemeral run's id is the workspace's to derive.
func (s *Session) Client(ctx context.Context) (any, error) {
	info, err := s.Info(ctx)
	if err != nil {
		return nil, err
	}
	return proxy.Client{Client: info.Client}, nil
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
	s.closeWatcher()
	s.closing.Do(func() {
		// Serving ends first: `remote stop` waits for the endpoint to go
		// quiet, and nothing should start in a session that is finishing.
		if s.listener != nil {
			_ = s.listener.Close()
		}
		// After the watcher, so no event can arm the invalidation timer again.
		if s.cache != nil {
			s.settleWriteBack()
			s.cache.Stop()
		}
	})
	s.once.Do(func() {
		s.cancel()
		s.gate.close()
		s.wg.Wait()
	})
	return nil
}

// settleTimeout bounds write-back's last round when a session closes. Long
// enough for a job's output over a slow link; what is left after it stays in
// the workspace, and the next session carries it back.
const settleTimeout = 60 * time.Second

// settleWriteBack carries back what a container changed before the session
// goes (ADR 0044, ADR 0027). A connection released while idle is opened again
// for it: the changes are in the workspace's cache layer whether or not a
// connection is up, and a stop that skipped them would leave a job's last
// output there. The lease keeps the idle sweep from releasing the connection
// under the round, and the context is the session's own, never the caller's.
func (s *Session) settleWriteBack() {
	if !s.cache.WritesBack() {
		return
	}
	// A machine workspace let go of stops (ADR 0026), and its unions go with
	// it: dialling would boot it only to find nothing to carry.
	if _, held := s.gate.currentLive(); !held && s.opts.Config.Machine != nil {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, settleTimeout)
	defer cancel()
	_, done, err := s.gate.acquire(ctx)
	if err != nil {
		s.log().Warn("could not reach the workspace to carry back the last changes; they stay there for the next session",
			"err", err)
		return
	}
	defer done()
	s.cache.Settle(ctx)
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
		case !r.Done && r.FillErr != nil:
			what = "filling, and retrying at " + r.FillRetry.Format(time.TimeOnly) + " after: " + r.FillErr.Error()
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
