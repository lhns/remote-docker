package main

import (
	"context"
	"time"

	"github.com/lhns/remote-docker/client/internal/session"
)

// idleExpired closes its channel once the session has had nothing to do for
// the given period AND nothing depends on it.
//
// Both conditions, always. Ending the process takes the NFS export with it,
// and a container holding one of our volumes loses its filesystem, so this
// asks the same question a connection release asks, for the same reason, and
// treats "cannot tell" as a reason to stay.
//
// A negative period disables it, matching how IdleTimeout spells the same
// idea.
func idleExpired(ctx context.Context, s *session.Session, idle time.Duration) <-chan struct{} {
	expired := make(chan struct{})
	if idle <= 0 {
		return expired // never closed: nil would block too, but this says why
	}

	go func() {
		// Checked several times per period rather than once at the end, so
		// the answer is at most a fraction of the period stale. Bounded below
		// because a very short period (which is what a test sets) must not
		// become a busy loop.
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
