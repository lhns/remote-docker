// What the session says about itself, and when it may be let go.
//
// Two audiences and one answer. The idle sweeper asks whether the CONNECTION
// can be dropped; `remote-docker status` and the daemon's own expiry ask
// whether the PROCESS can end. Both come down to hasLiveDependents, and the
// consequences differ by a lot: a dropped connection reopens on the next
// request, an ended process takes the NFS export with it and a running
// container's filesystem with that.

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
			s.releaseIfIdle()
		}
	}
}

func (s *Session) releaseIfIdle() {
	s.gate.sweep(s.ctx)
}

// hasLiveDependents reports whether anything running still needs us.
//
// Two things can, and both must be checked. A container holding one of our
// volumes has a live NFS mount that dropping the tunnel would break, and a
// running container of ours may have published ports whose forwards exist only
// while we are connected.
//
// Interactive sessions are deliberately NOT counted here. Every stream holds
// its gate lease for its whole life, and the sweep stops at users > 0 long
// before it asks this, so a separate counter would state an intent the gate
// already enforces.
//
// The volume match is scoped to volumes WE created, never to the `rd-` prefix
// alone. On a shared daemon (ADR 0012) that prefix also matches other
// accounts' volumes, and one of those pins this connection open forever: an
// idle release that can never fire, waiting on a dependency that is not ours.
func (s *Session) hasLiveDependents(ctx context.Context, live *liveConn) (bool, error) {
	containers, err := live.api.ListContainers(ctx)
	if err != nil {
		return false, err
	}
	ours := s.ourVolumes()
	for _, c := range containers {
		// This account's containers, started from THIS machine. Scoped by
		// client as well, because one account used from two computers labels
		// both the same: without it, machine A could never release its
		// connection while machine B had anything running, which is ADR 0015's
		// idle release quietly becoming unreachable.
		//
		// A container with no client label was started before machines were
		// named, and counts: it may well be this one's.
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

// ourVolumes names the volumes backing this session's shares.
//
// Derived rather than remembered: share ids are a pure function of the local
// path (ADR 0007), so the registry already knows the exact set and no round
// trip is needed to ask.
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

	// Last: the machine may go away once nothing is holding it, and everything
	// above wanted it there.
	if live.machine != nil {
		_ = live.machine.Close()
	}
}

// CollectOptions widens what a collection is allowed to remove.
type CollectOptions struct {
	// Orphans also removes unused share volumes that name no machine.
	//
	// Those are what a version before machines were named left behind, or what
	// this machine left when its key was replaced. Asked for rather than
	// assumed, because "names no machine" is not "mine": another of this
	// account's machines running an older build may still be using one.
	Orphans bool
}

// Collect removes share volumes this account is no longer using.
func (s *Session) Collect(ctx context.Context, opts ...CollectOptions) (int, error) {
	live, done, err := s.acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer done()

	collector := s.collector(live)
	if len(opts) > 0 && opts[0].Orphans {
		// Widened to volumes naming no machine, and NOT to every machine's:
		// clearing Client entirely would collect the other computer's, which
		// is the failure the scoping exists to prevent.
		collector.Orphans = true
	}

	n, err := collector.Collect(ctx)
	if err == nil {
		s.pruneShareRecord(ctx, live)
	}
	return n, err
}

// pruneShareRecord drops what this workspace no longer has a volume for.
//
// The record exists to answer a mount, so an entry whose volume is gone can
// never be asked for again. Best effort and after the collection: failing to
// tidy a record is not a reason to report a collection that happened as a
// failure.
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
		Remover: live.api,
		InUse:   live.api,
		Owner:   live.info.User,
		Client:  s.clientID,
		Guard:   live.guard,
		Caches:  s.mountedCaches,
		Log:     s.opts.Log,
	}
}

// mountedCaches asks the workspace which cache volumes it has a union on.
//
// The collector's one question about a cache volume that neither the daemon nor
// this session can answer: a union is bound by path, so no container references
// the volume, and a share prepared by an EARLIER session is not in this one's
// registry at all (ADR 0044).
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

// exportsVolume reports whether a managed volume backs a directory this
// session is exporting right now.
//
// The registry is the only place that knows: the volume exists on the
// workspace from the moment a bind is rewritten, and the daemon does not call
// it in use until a container names it. Everything between those two is a
// volume that must survive collection.
func (s *Session) exportsVolume(volume string) bool {
	for _, share := range s.registry.Shares() {
		name, err := workspace.VolumeNameForExport(s.clientID, share.ExportPath)
		if err != nil {
			continue
		}
		if name == volume {
			return true
		}
	}
	return false
}

// Status answers the control endpoint, satisfying proxy.Control.
//
// Deliberately does NOT connect. `status` connecting is its own decision --
// reporting what the workspace says is that command's whole job, but a
// daemon asked to describe itself must not go and establish a connection it
// had let go, which would make asking the question change the answer.
func (s *Session) Status() any {
	// currentLive, not current: a session holding a connection that has died
	// is not connected, and saying so is the difference between `status`
	// reporting the truth and reporting a field.
	live, connected := s.gate.currentLive()
	st := proxy.Status{
		Version:   s.opts.Version,
		Workspace: s.opts.Config.Name,
		Host:      s.opts.Config.Host,
		User:      s.opts.Config.User,
		Endpoint:  s.Endpoint,
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
		st.User = live.info.User
		st.Storage = live.info.Storage
		st.Ports = s.Ports()
	}
	if s.watch != nil {
		st.Watching = s.watch.Stats().Mode.String()
	}
	for _, share := range s.registry.Shares() {
		st.Shares = append(st.Shares, share.LocalPath)
	}
	st.Caches = s.cacheStatus()
	return st
}

// Idle reports whether this session could be ended without breaking anything,
// satisfying proxy.Control.
func (s *Session) Idle() any {
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()

	quiet, safe := s.IdleFor(ctx)
	return proxy.Idle{Safe: safe, Quiet: quiet.Round(time.Second).String()}
}

// Shutdown asks the session to stop, satisfying proxy.Control.
//
// Returns immediately and stops in the background, because the caller is the
// control request still holding a connection that Close is about to shut.
func (s *Session) Shutdown() {
	go func() {
		s.stopOnce.Do(func() { close(s.stopped) })
	}()
}

// IdleFor reports how long this session has had nothing to do, and whether it
// would be safe to end the process now.
//
// Safe means the same thing it means for releasing a connection, because the
// consequence is worse: a released connection reopens on the next request, and
// an ended process takes the NFS export with it and a running container's
// filesystem with that.
//
// The disjunction is the load-bearing part. If no connection is held, the gate
// only let it go BECAUSE nothing depended on it, so there is nothing to ask
// and nothing to break. If one is held, ask, and "unable to tell" counts as
// busy, exactly as it does for a release.
func (s *Session) IdleFor(ctx context.Context) (time.Duration, bool) {
	last, inUse := s.gate.lastUse()
	if inUse {
		return 0, false
	}
	// Never used means idle since the session began, not idle for no time at
	// all. Reading the zero time as "just now" meant a daemon that had served
	// nothing could never expire, which is the case where reclaiming it is most
	// obviously right, and the case `start` leaves behind every time somebody
	// opens a session and then does not use it.
	if last.IsZero() {
		last = s.started
	}
	quiet := time.Since(last)

	// currentLive, so a dead connection takes the "nothing depends on this"
	// branch instead of being asked over a transport that cannot answer.
	// Asking it anyway is what makes `remote restart` refuse on exactly the
	// session that most needs restarting, leaving --force as the only way out.
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

// Stopped is closed when something has asked this session to stop. `up` waits
// on it alongside its signal context.
func (s *Session) Stopped() <-chan struct{} { return s.stopped }

// Ports lists the ports currently forwarded, if connected.
func (s *Session) Ports() []int {
	live, ok := s.gate.current()
	if !ok || live.ports == nil {
		return nil
	}
	return live.ports.Active()
}

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

// humanBytes is a size somebody can read at a glance.
//
// A byte count is the more useful half of "how much of this is cached": a share
// can be most of its files and a fraction of its bytes, or the other way round,
// and which one it is decides whether the cache is worth anything.
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

// cacheStatus is one line per delegated share, saying how much of it is cached.
//
// A fraction rather than a verdict: over the budget, still filling, and
// complete are all states a share works in, and the difference between them is
// how much of it is local rather than whether it is right.
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
			// Over the budget, or a walk that could not read part of the tree.
			// The rest is served from the live mount, which is slower and
			// right, and saying so is the only way anybody would know.
			what = "cached in part; the rest is read live"
		}

		// "N of M" only once M is known. Stats lands when the walk finishes, so
		// while a fill runs TotalFiles is 0 and the fraction reads "512 of 0
		// files" -- a number that looks like a bug in the thing it is
		// reporting on.
		if r.Done {
			line := fmt.Sprintf("%s: %d of %d files, %s of %s, %s sent, %s",
				local, r.Sent, stats.TotalFiles,
				humanBytes(stats.Bytes), humanBytes(stats.TotalBytes), humanBytes(r.Bytes), what)
			// A cache that mysteriously omits .git is worth being able to
			// explain, which is the only reason the walk counts these.
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
