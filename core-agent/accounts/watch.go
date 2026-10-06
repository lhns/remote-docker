package accounts

import (
	"context"
	"time"

	"github.com/fsnotify/fsnotify"
)

// defaultPollInterval is how often the keys directories are re-read regardless
// of notifications.
const defaultPollInterval = 60 * time.Second

// Watch keeps accounts in step with the keys directories until ctx is done.
//
// It both watches and polls, and the polling is not belt-and-braces. A keys
// directory is expected to live on shared storage (CephFS, NFS) where inotify
// never fires for a change made on another host. A deployment where enrolment
// happens from a management node would silently never see a new key.
//
// This is the same lesson ADR 0014 records from the other side: a network
// filesystem carries no change notification, so anything depending on one must
// poll.
func (s *Store) Watch(ctx context.Context, poll time.Duration) error {
	if poll <= 0 {
		poll = defaultPollInterval
	}

	// Synced once up front so the agent has its accounts before it accepts a
	// single connection.
	if err := s.Sync(); err != nil {
		return err
	}

	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	// A directory that cannot be watched is still polled. Not fatal: polling
	// alone is a complete implementation, just a slower one.
	var events <-chan fsnotify.Event
	var errs <-chan error
	if watcher, err := fsnotify.NewWatcher(); err != nil {
		s.log().Warn("cannot watch the keys directories; polling instead", "err", err, "every", poll)
	} else {
		defer func() { _ = watcher.Close() }()
		for _, dir := range s.dirs() {
			if err := watcher.Add(dir); err != nil {
				s.log().Warn("cannot watch a keys directory; polling it instead",
					"dir", dir, "err", err, "every", poll)
			}
		}
		events, errs = watcher.Events, watcher.Errors
	}

	// Changes arrive in bursts: an editor writing a key file produces
	// several events, so a change schedules one sync shortly after rather
	// than one per event.
	var pending <-chan time.Time

	for {
		select {
		case <-ctx.Done():
			return nil

		// Every event, dotfiles included: a Kubernetes Secret volume changes
		// by swapping its `..data` link.
		case _, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if pending == nil {
				pending = time.After(250 * time.Millisecond)
			}

		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			s.log().Warn("watching the keys directories", "err", err)

		case <-pending:
			pending = nil
			s.syncLogged()

		case <-ticker.C:
			s.syncLogged()
		}
	}
}

// syncLogged reports a failed sync and carries on. A broken keys directory
// should not stop the agent serving the accounts it already has. It does not
// wait for provisioning, which publishes each account itself.
func (s *Store) syncLogged() {
	if _, err := s.sync(); err != nil {
		s.log().Error("syncing accounts", "err", err)
	}
}
