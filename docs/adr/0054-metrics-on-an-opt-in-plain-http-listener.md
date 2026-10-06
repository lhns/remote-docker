# 0054 — Metrics on an opt-in plain HTTP listener, in the text format, with no client library

- Status: Accepted
- Date: 2026-10-06

## Context

- Ephemeral clients (ADR 0050) asked for metrics: how many runs, in which
  state, what was refused and what cleanup removed or kept. Those were only in
  the log.
- The agent counts its third-party requires (ADR 0021). `client_golang` brings
  `client_model`, `common`, `procfs`, protobuf and more for a dozen series.
- The SSH and WebSocket ports are authenticated by the SSH handshake; an HTTP
  endpoint on either would be the first unauthenticated thing on them.

## Decision

- **`agent/internal/metrics` writes the Prometheus text exposition format
  0.0.4 with the stdlib**: `# HELP`, `# TYPE`, label values escaped, families
  and samples sorted, `Content-Type: text/plain; version=0.0.4; charset=utf-8`.
  Counters and gauges only. The agent's `go.mod` does not change.
- **Opt-in**: `WORKSPACE_METRICS_ADDR` (e.g. `:9090`), unset by default, read
  once at start. It serves `GET /metrics` in plain HTTP on its own listener,
  never the SSH or WebSocket port. An address that cannot be bound stops the
  start, naming the variable.
- **Gauges are read at scrape time** from what already holds the answer: the
  ephemeral registry (`Census`), the revoke registry (`Server.Connections`)
  and the daemon manager (`Started`). No parallel state.
- **Counters are injected** as `*metrics.Counter` fields, incremented where the
  event happens; a nil one counts nothing, so tests and callers that do not
  wire metrics are unchanged.
- **Labels are account names and fixed words**, never a key, a client id or a
  token id: those are secrets or unbounded.
- **The chart**: `metrics.enabled` and `metrics.port`, a container port named
  `metrics` and the `prometheus.io/scrape`, `/port`, `/path` pod annotations.
  No ServiceMonitor: it needs the Prometheus operator's CRD, and the chart
  calls no Kubernetes API (ADR 0035).

The metric names are listed in the README under "Metrics".

## Consequences

- No histograms, no process or Go runtime metrics. Adding those is the point
  at which `client_golang` should be weighed again.
- The endpoint is unauthenticated. Account names are visible to whoever can
  reach it, so it belongs on a network only the scraper reaches.
- `remote_docker_account_daemons` counts daemons started or adopted, not
  probed: asking each daemon per scrape is a `docker inspect` each. One that
  died is counted until it is reset.
