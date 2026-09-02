package main

import (
	"testing"
	"time"

	"github.com/lhns/remote-docker/client/internal/config"
)

// Zero and negative are different answers, and a deployment that wanted "never"
// has already picked the wrong one: zero selects the thirty-minute default, so
// the endpoint went away half an hour later.
//
// Both spellings are load-bearing in the field. A CI runner sets -1s so its
// endpoint outlives an idle period no job arrives in; anything unset gets the
// default so an interactive session still reclaims itself.
func TestDaemonIdle(t *testing.T) {
	for _, c := range []struct {
		name       string
		configured time.Duration
		want       time.Duration
	}{
		{"unset means the default", 0, config.DefaultDaemonIdle},
		{"a value means itself", 5 * time.Minute, 5 * time.Minute},
		{"negative means never, and is preserved", -time.Second, -time.Second},
	} {
		if got := daemonIdle(c.configured); got != c.want {
			t.Errorf("%s: daemonIdle(%v) = %v, want %v", c.name, c.configured, got, c.want)
		}
	}
}

// "Never" has to mean never, and it is the one setting a runner depends on:
// with the endpoint gone, every `docker` call in every later job fails until
// the pod is restarted, while the pod stays healthy and says nothing.
//
// idleExpired answers before it touches the session for this case, which is why
// there is no session here to give it.
func TestIdleNeverExpiresWhenDisabled(t *testing.T) {
	for _, idle := range []time.Duration{0, -time.Second, -time.Hour} {
		expired := idleExpired(t.Context(), nil, idle)
		select {
		case <-expired:
			t.Errorf("idle=%v reported the session expired", idle)
		case <-time.After(50 * time.Millisecond):
		}
	}
}
