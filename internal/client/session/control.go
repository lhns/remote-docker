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
	"os"
	"time"

	"github.com/lhns/remote-docker/internal/client/proxy"
	"github.com/lhns/remote-docker/internal/client/rewrite"
	"github.com/lhns/remote-docker/pkg/workspace"
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
// A third used to be listed here -- an interactive shell using the ~/workspace
// mount -- and was counted separately on liveConn. It never reached this
// function: Shell holds its gate lease for its whole life, and sweep bails on
// users > 0 long before busy is consulted. Now that every stream holds its
// lease the same way, the counter documented an intent the code no longer
// needed, so it is gone.
//
// The volume match is scoped to volumes WE created. It used to accept any
// rd- prefix, so on a shared daemon (ADR 0012) another account's volume pinned
// this connection open forever -- an idle release that could never fire, for a
// dependency that was not ours.
func (s *Session) hasLiveDependents(ctx context.Context, live *liveConn) (bool, error) {
	containers, err := live.api.ListContainers(ctx)
	if err != nil {
		return false, err
	}
	ours := s.ourVolumes()
	for _, c := range containers {
		if c.Labels[rewrite.OwnerLabel] == live.info.User {
			return true, nil
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
		if name, err := workspace.VolumeNameForExport(share.ExportPath); err == nil {
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
}

// Collect removes share volumes this account is no longer using.
func (s *Session) Collect(ctx context.Context) (int, error) {
	live, done, err := s.acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer done()
	return s.collector(live).Collect(ctx)
}

func (s *Session) collector(live *liveConn) *rewrite.Collector {
	return &rewrite.Collector{
		Volumes: live.api,
		Remover: live.api,
		InUse:   live.api,
		Owner:   live.info.User,
		Log:     s.opts.Log,
	}
}

// Status answers the control endpoint, satisfying proxy.Control.
//
// Deliberately does NOT connect. `status` connecting is its own decision --
// reporting what the workspace says is that command's whole job -- but a
// daemon asked to describe itself must not go and establish a connection it
// had let go, which would make asking the question change the answer.
func (s *Session) Status() any {
	live, connected := s.gate.current()
	st := proxy.Status{
		Version:   s.opts.Version,
		Workspace: s.opts.Config.Name,
		Host:      s.opts.Config.Host,
		User:      s.opts.Config.User,
		Endpoint:  s.Endpoint,
		PID:       os.Getpid(),
		Connected: connected,
		Since:     s.started.Format(time.RFC3339),
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
// and nothing to break. If one is held, ask -- and "unable to tell" counts as
// busy, exactly as it does for a release.
func (s *Session) IdleFor(ctx context.Context) (time.Duration, bool) {
	last, inUse := s.gate.lastUse()
	if inUse {
		return 0, false
	}
	// Never used means idle since the session began, not idle for no time at
	// all. Reading the zero time as "just now" meant a daemon that had served
	// nothing could never expire -- the one case where reclaiming it is most
	// obviously right, and the case `start` leaves behind every time somebody
	// opens a session and then does not use it.
	if last.IsZero() {
		last = s.started
	}
	quiet := time.Since(last)

	live, connected := s.gate.current()
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
