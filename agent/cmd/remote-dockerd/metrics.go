package main

import (
	"fmt"
	"net"

	"github.com/lhns/remote-docker/agent/internal/daemons"
	"github.com/lhns/remote-docker/agent/internal/ephemeral"
	"github.com/lhns/remote-docker/agent/internal/metrics"
	"github.com/lhns/remote-docker/agent/internal/sshd"
)

// envMetricsAddr is where GET /metrics is served, in plain HTTP on a listener
// of its own (ADR 0054). Unset serves nothing.
const envMetricsAddr = "WORKSPACE_METRICS_ADDR"

// listenMetrics opens the metrics listener, or none for an empty addr.
func listenMetrics(addr string) (net.Listener, error) {
	ln, err := metrics.Listen(addr)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", envMetricsAddr, err)
	}
	return ln, nil
}

// agentMetrics are every metric the agent exposes. Label values are account
// names and fixed words, never a key, a client id or a token id.
type agentMetrics struct {
	reg metrics.Registry

	runsRefused, sweeps, removed, kept *metrics.Counter
	redemptions, limiter               *metrics.Counter
}

func newAgentMetrics() *agentMetrics {
	m := &agentMetrics{}
	m.runsRefused = m.reg.Counter("remote_docker_ephemeral_runs_refused_total",
		"Ephemeral runs refused, by reason.", "reason")
	m.sweeps = m.reg.Counter("remote_docker_ephemeral_sweeps_total",
		"Sweeps of the ephemeral runs for expired ones.")
	m.removed = m.reg.Counter("remote_docker_ephemeral_objects_removed_total",
		"Objects an expired ephemeral run left, removed by cleanup, by kind.", "kind")
	m.kept = m.reg.Counter("remote_docker_ephemeral_objects_kept_total",
		"Objects an expired ephemeral run left, kept by cleanup, by kind and reason.", "kind", "reason")
	m.redemptions = m.reg.Counter("remote_docker_token_redemptions_total",
		"Enrolment token redemptions, by outcome.", "outcome")
	m.limiter = m.reg.Counter("remote_docker_token_limiter_rejections_total",
		"Redemptions turned away by the global failed-attempt limiter.")

	// Every fixed series from the start, so a rate over it is defined.
	for _, r := range []string{ephemeral.RefusedLimit, ephemeral.RefusedCleaning, sshd.RefusedDuplicate, sshd.RefusedMalformed} {
		m.runsRefused.Add(0, r)
	}
	for _, k := range []string{ephemeral.KindContainer, ephemeral.KindNetwork, ephemeral.KindVolume} {
		m.removed.Add(0, k)
	}
	for _, o := range []string{sshd.RedeemOK, sshd.RedeemPending, sshd.RedeemRefused, sshd.RedeemFailed} {
		m.redemptions.Add(0, o)
	}
	m.sweeps.Add(0)
	m.limiter.Add(0)
	return m
}

// gauges registers what is read at scrape time. manager is nil with a shared
// daemon, which has no per-account daemons to count.
func (m *agentMetrics) gauges(runs *ephemeral.Registry, ephemeralAccounts map[string]bool, server *sshd.Server, manager *daemons.Manager) {
	m.reg.Gauge("remote_docker_ephemeral_runs",
		"Ephemeral runs, by account and state.", []string{"account", "state"}, func() []metrics.Sample {
			census, _ := runs.Census()
			for account := range ephemeralAccounts {
				for _, s := range []ephemeral.State{ephemeral.Live, ephemeral.Grace, ephemeral.Cleaning} {
					census[ephemeral.Group{Account: account, State: s}] += 0
				}
			}
			out := make([]metrics.Sample, 0, len(census))
			for g, n := range census {
				out = append(out, metrics.Sample{Labels: []string{g.Account, g.State.String()}, Value: float64(n)})
			}
			return out
		})
	m.reg.Gauge("remote_docker_ephemeral_run_ports",
		"Ports held by ephemeral runs.", nil, func() []metrics.Sample {
			_, ports := runs.Census()
			return []metrics.Sample{{Value: float64(ports)}}
		})
	m.reg.Gauge("remote_docker_ssh_connections",
		"Live SSH connections logged in as an account, by account.", []string{"account"}, func() []metrics.Sample {
			var out []metrics.Sample
			for account, n := range server.Connections() {
				out = append(out, metrics.Sample{Labels: []string{account}, Value: float64(n)})
			}
			return out
		})
	if manager != nil {
		m.reg.Gauge("remote_docker_account_daemons",
			"Per-account daemons this agent has started or adopted; not probed, so one that died is still counted.",
			nil, func() []metrics.Sample {
				return []metrics.Sample{{Value: float64(manager.Started())}}
			})
	}
}
