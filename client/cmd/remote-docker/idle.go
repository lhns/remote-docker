package main

import (
	"context"
	"time"

	"github.com/lhns/remote-docker/client/internal/session"
)

// idleExpired closes its channel once the session has been quiet for idle AND
// nothing depends on it: exiting takes the NFS export from any container using
// it. "Cannot tell" means stay. A non-positive period disables it.
func idleExpired(ctx context.Context, s *session.Session, idle time.Duration) <-chan struct{} {
	expired := make(chan struct{})
	if idle <= 0 {
		return expired // never closed: nil would block too, but this says why
	}

	go func() {
		interval := max(idle/4, time.Second)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				quiet, safe := s.IdleFor(ctx)
				if safe && quiet >= idle {
					close(expired)
					return
				}
			}
		}
	}()
	return expired
}

// standbyWhenIdle lets go of the workspace whenever nothing has needed it,
// repeatedly: Session.Standby is idempotent and a request wakes the session.
func standbyWhenIdle(ctx context.Context, s *session.Session, standby time.Duration) {
	if standby <= 0 {
		return
	}
	ticker := time.NewTicker(max(standby/4, time.Second))
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if quiet, safe := s.IdleFor(ctx); safe && quiet >= standby {
				s.Standby()
			}
		}
	}
}
