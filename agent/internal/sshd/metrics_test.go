package sshd

import (
	"maps"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lhns/remote-docker/agent/internal/metrics"
	"github.com/lhns/remote-docker/core/enrol"
)

func withMetrics(m *Metrics) func(*Config) {
	var reg metrics.Registry
	m.Redemptions = reg.Counter("redemptions_total", "", "outcome")
	m.LimiterRejections = reg.Counter("limiter_rejections_total", "")
	m.RunsRefused = reg.Counter("runs_refused_total", "", "reason")
	return func(cfg *Config) { cfg.Metrics = *m }
}

func TestRedemptionsAreCountedByOutcome(t *testing.T) {
	var m Metrics
	w := startTokenWorkspace(t, "", withMetrics(&m))
	id, secret := w.mint(t, "alice")
	wrong := strings.Repeat("A", len(secret))

	for range w.limiter.Burst {
		w.redeem(t, id, newSigner(t), enrol.RedeemRequest{Secret: wrong})
	}
	wantCode(t, w.redeem(t, id, newSigner(t), enrol.RedeemRequest{Secret: secret}), enrol.CodeBusy)

	ok := startTokenWorkspace(t, "", func(cfg *Config) { cfg.Metrics = m })
	id, secret = ok.mint(t, "bob")
	if reply := ok.redeem(t, id, newSigner(t), enrol.RedeemRequest{Secret: secret}); reply.Error != nil {
		t.Fatalf("redeem: %+v", reply.Error)
	}

	broken := startTokenWorkspace(t, filepath.Join(t.TempDir(), "missing"), func(cfg *Config) { cfg.Metrics = m })
	id, secret = broken.mint(t, "carol")
	wantCode(t, broken.redeem(t, id, newSigner(t), enrol.RedeemRequest{Secret: secret}), enrol.CodeStorage)

	for outcome, want := range map[string]float64{
		RedeemRefused: float64(w.limiter.Burst) + 1,
		RedeemOK:      1,
		RedeemFailed:  1,
		RedeemPending: 0,
	} {
		if v := m.Redemptions.Value(outcome); v != want {
			t.Errorf("redemptions{%s} = %v, want %v", outcome, v, want)
		}
	}
	if v := m.LimiterRejections.Value(); v != 1 {
		t.Errorf("limiter rejections = %v, want 1", v)
	}
}

func TestRedeemOutcome(t *testing.T) {
	for _, tc := range []struct {
		reply enrol.RedeemReply
		want  string
	}{
		{enrol.RedeemReply{Account: "alice"}, RedeemOK},
		{enrol.RedeemReply{Account: "alice", Pending: true}, RedeemPending},
		{enrol.RedeemReply{Error: enrol.Refused}, RedeemRefused},
		{enrol.RedeemReply{Error: &enrol.Error{Code: enrol.CodeName}}, RedeemRefused},
		{enrol.RedeemReply{Error: &enrol.Error{Code: enrol.CodeBusy}}, RedeemRefused},
		{enrol.RedeemReply{Error: &enrol.Error{Code: enrol.CodeStorage}}, RedeemFailed},
		{enrol.RedeemReply{Error: &enrol.Error{Code: enrol.CodeFailed}}, RedeemFailed},
	} {
		if got := redeemOutcome(tc.reply); got != tc.want {
			t.Errorf("redeemOutcome(%+v) = %s, want %s", tc.reply, got, tc.want)
		}
	}
}

func TestRunRefusalsAreCounted(t *testing.T) {
	var m Metrics
	w := startRunWorkspace(t, withMetrics(&m))
	c := w.dial(t, "alice", w.alice, runA)
	if ok, _ := sendRun(t, c, runB); ok {
		t.Fatal("a second run on one connection was accepted")
	}
	if ok, _ := sendRun(t, w.dial(t, "alice", w.alice, ""), "not-a-run"); ok {
		t.Fatal("a malformed run was accepted")
	}
	if v := m.RunsRefused.Value(RefusedDuplicate); v != 1 {
		t.Errorf("runs refused{duplicate} = %v, want 1", v)
	}
	if v := m.RunsRefused.Value(RefusedMalformed); v != 1 {
		t.Errorf("runs refused{malformed} = %v, want 1", v)
	}
}

func TestConnectionsCountsLoggedInAccounts(t *testing.T) {
	w := startRevokeWorkspace(t)
	alice, bob := newSigner(t), newSigner(t)
	w.enrol(t, "alice", alice)
	w.enrol(t, "bob", bob)

	a1 := w.dial(t, "alice", alice)
	w.dial(t, "alice", alice)
	w.dial(t, "bob", bob)
	if got, want := w.server.Connections(), map[string]int{"alice": 2, "bob": 1}; !maps.Equal(got, want) {
		t.Errorf("connections = %v, want %v", got, want)
	}

	_ = a1.Close()
	want := map[string]int{"alice": 1, "bob": 1}
	deadline := time.Now().Add(5 * time.Second)
	for !maps.Equal(w.server.Connections(), want) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := w.server.Connections(); !maps.Equal(got, want) {
		t.Errorf("after a close, connections = %v, want %v", got, want)
	}
}
