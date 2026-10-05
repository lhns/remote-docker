package tokens

import (
	"sync"
	"time"
)

// Limiter bounds failed redemptions across the whole workspace. Global rather
// than per address, because behind an ingress every connection arrives from
// the same one. Only failures spend it, so an honest redemption is never
// slowed by another.
type Limiter struct {
	Burst int
	Every time.Duration // one failure refunded per interval

	// Now is the clock, for tests. Nil is time.Now.
	Now func() time.Time

	mu    sync.Mutex
	spent float64
	at    time.Time
}

// NewLimiter is the workspace's: 10 failures at once, then one per 6s.
func NewLimiter() *Limiter { return &Limiter{Burst: 10, Every: 6 * time.Second} }

// Allow reports whether a redemption may be attempted now.
func (l *Limiter) Allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refill()
	return l.spent+1 <= float64(l.Burst)
}

// Fail spends one attempt.
func (l *Limiter) Fail() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refill()
	l.spent++
}

func (l *Limiter) refill() {
	now := time.Now()
	if l.Now != nil {
		now = l.Now()
	}
	if !l.at.IsZero() && l.Every > 0 {
		l.spent = max(0, l.spent-float64(now.Sub(l.at))/float64(l.Every))
	}
	l.at = now
}
