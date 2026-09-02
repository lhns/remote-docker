package main

import (
	"testing"
	"time"

	"github.com/lhns/remote-docker/client/internal/config"
)

// Zero and negative are different answers, and a deployment that wanted "never"
// picked the wrong one: zero takes the default, which was thirty minutes, so
// the endpoint went away half an hour later.
//
// They still differ now the default is never -- an explicit duration reclaims
// and an unset one does not -- which is the distinction the runner depends on.
func TestDaemonIdle(t *testing.T) {
	for _, c := range []struct {
		name       string
		configured time.Duration
		want       time.Duration
	}{
		{"unset means the default", 0, config.DefaultDaemonIdle},
		{"and the default is never", 0, config.DaemonIdleNever},
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

// The fix, asserted where it matters: an unconfigured session must not reclaim
// itself, because the reclaim takes the endpoint and every foreign Docker
// client pointed at it.
//
// Set deliberately it still works, which is the other half -- the option did
// not go away, it stopped being what nobody chose.
func TestTheDefaultSessionNeverReclaimsItself(t *testing.T) {
	expired := idleExpired(t.Context(), nil, daemonIdle(0))
	select {
	case <-expired:
		t.Fatal("an unconfigured session reclaimed itself, taking the endpoint with it")
	case <-time.After(50 * time.Millisecond):
	}
}

// The two tiers have different defaults on purpose: letting go of the workspace
// is safe to do unasked, ending the process is not.
func TestTheTwoIdleTiersDefaultDifferently(t *testing.T) {
	if got := daemonStandby(0); got != config.DefaultDaemonStandby {
		t.Errorf("standby default = %v, want %v", got, config.DefaultDaemonStandby)
	}
	if daemonStandby(0) <= 0 {
		t.Error("standby is disabled by default, so nothing is ever reclaimed")
	}
	if daemonIdle(0) > 0 {
		t.Error("shutdown is enabled by default, which takes the endpoint with it")
	}
	if got := daemonStandby(-time.Second); got != -time.Second {
		t.Errorf("negative standby = %v, want it preserved", got)
	}
}
