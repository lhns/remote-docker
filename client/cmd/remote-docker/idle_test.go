package main

import (
	"testing"
	"time"

	"github.com/lhns/remote-docker/client/internal/config"
)

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

// An unconfigured session must not reclaim itself, because the reclaim takes
// the endpoint and every foreign Docker client pointed at it.
func TestTheDefaultSessionNeverReclaimsItself(t *testing.T) {
	expired := idleExpired(t.Context(), nil, config.DefaultDaemonIdle)
	select {
	case <-expired:
		t.Fatal("an unconfigured session reclaimed itself, taking the endpoint with it")
	case <-time.After(50 * time.Millisecond):
	}
}

// The two tiers have different defaults on purpose: letting go of the workspace
// is safe to do unasked, ending the process is not.
func TestTheTwoIdleTiersDefaultDifferently(t *testing.T) {
	if config.DefaultDaemonStandby <= 0 {
		t.Error("standby is disabled by default, so nothing is ever reclaimed")
	}
	if config.DefaultDaemonIdle > 0 {
		t.Error("shutdown is enabled by default, which takes the endpoint with it")
	}
}
