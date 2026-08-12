package accounts

import (
	"context"
	"time"

	"github.com/fsnotify/fsnotify"
)

// DefaultPollInterval is how often the keys directory is re-read regardless of
// notifications.
const DefaultPollInterval = 60 * time.Second

// Watch keeps accounts in step with the keys directory until ctx is done.
//
// It both watches and polls, and the polling is not belt-and-braces. The keys
// directory is expected to live on shared storage (CephFS, NFS) where
// inotify never fires for a change made on another host. A deployment where
// enrolment happens from a management node would silently never see a new key.
//
// This is the same lesson ADR 0014 records from the other side: a network
// filesystem carries no change notification, so anything depending on one must
// poll.
func (s *Store) Watch(ctx context.Context, poll time.Duration) error {
	if poll <= 0 {
		poll = DefaultPollInterval
	}

	// Synced once up front so the agent has its accounts before it accepts a
	// single connection.
	if err := s.Sync(); err != nil {
		return err
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		// Not fatal: polling alone is a complete implementation, just a
		// slower one. Refusing to start over this would be worse.
		s.log().Warn("cannot watch the keys directory; polling instead",
			"dir", s.KeysDir, "err", err, "every", poll)
		s.pollOnly(ctx, poll)
		return nil
	}
	defer func() { _ = watcher.Close() }()

	if err := watcher.Add(s.KeysDir); err != nil {
		s.log().Warn("cannot watch the keys directory; polling instead",
			"dir", s.KeysDir, "err", err, "every", poll)
		s.pollOnly(ctx, poll)
		return nil
	}

	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	// Changes arrive in bursts: an editor writing a key file produces
	// several events, so a change schedules one sync shortly after rather
	// than one per event.
	var pending <-chan time.Time

	for {
		select {
		case <-ctx.Done():
			return nil

		case <-watcher.Events:
			if pending == nil {
				pending = time.After(250 * time.Millisecond)
			}

		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			s.log().Warn("watching the keys directory", "dir", s.KeysDir, "err", err)

		case <-pending:
			pending = nil
			s.syncLogged()

		case <-ticker.C:
			s.syncLogged()
		}
	}
}

func (s *Store) pollOnly(ctx context.Context, poll time.Duration) {
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.syncLogged()
		}
	}
}

// syncLogged reports a failed sync and carries on. A broken keys directory
// should not stop the agent serving the accounts it already has.
func (s *Store) syncLogged() {
	if err := s.Sync(); err != nil {
		s.log().Error("syncing accounts", "err", err)
	}
}
