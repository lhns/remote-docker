# 0054 — Metrics on an opt-in plain HTTP listener, in the text format, with no client library

- Status: Accepted
- Date: 2026-10-06, amended 2026-10-06 (histograms, build info, process metrics), 2026-10-07 (readable from every account, accepted)
- Current answer: the stdlib writer in `agent/internal/metrics` serves
  counters, gauges and fixed-bucket histograms, plus `build_info` and the
  standard `process_*` and `go_*` metrics, on an opt-in unauthenticated
  listener. `client_golang` was weighed again for the histograms and stayed
  out; the agent's `go.mod` did not change.

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
  The agent's `go.mod` does not change.
- **Opt-in**: `WORKSPACE_METRICS_ADDR` (e.g. `:9090`), unset by default, read
  once at start. It serves `GET /metrics` in plain HTTP on its own listener,
  never the SSH or WebSocket port. An address that cannot be bound stops the
  start, naming the variable.
- **Gauges are read at scrape time** from what already holds the answer: the
  ephemeral registry (`Census`), the revoke registry (`Server.Connections`)
  and the daemon manager (`Started`). No parallel state.
- **Counters and histograms are injected** as `*metrics.Counter` and
  `*metrics.Histogram` fields, updated where the event happens; a nil one
  records nothing, so tests and callers that do not wire metrics are
  unchanged. `core-agent/accounts` cannot import the agent's package, so
  `Store.ProvisionObserved` is a plain callback the agent wires.
- **Histograms** have fixed buckets chosen per metric, and are written as
  `client_golang` writes them: cumulative `_bucket{le}` ascending with `+Inf`,
  then `_sum` and `_count`. Four, in seconds: token redemption (by outcome),
  per-account daemon start (successful starts and restarts only, since a
  failure mostly measures `WORKSPACE_DAEMON_READY_TIMEOUT`), account
  provisioning (by outcome) and the ephemeral sweep.
- **`remote_docker_build_info{version,goversion}`** is a gauge of 1.
- **Process metrics under the standard names**, so stock dashboards find them:
  `go_goroutines`, `go_memstats_heap_alloc_bytes` (from `runtime/metrics`, no
  stop-the-world), and on Linux `process_cpu_seconds_total`,
  `process_resident_memory_bytes`, `process_open_fds` and
  `process_start_time_seconds`, read from `/proc/self` at scrape time.
  Elsewhere the `process_*` families are not registered. USER_HZ is taken as
  100, as `procfs` does (`const userHZ = 100` in `procfs/proc_stat.go`, read
  2026-10-06). Only these few: the rest of `client_golang`'s Go collector is
  what the library would be for.
- **Labels are account names and fixed words**, never a key, a client id or a
  token id: those are secrets or unbounded.
- **The chart**: `metrics.enabled` and `metrics.port`, a container port named
  `metrics` and the `prometheus.io/scrape`, `/port`, `/path` pod annotations.
  No ServiceMonitor: it needs the Prometheus operator's CRD, and the chart
  calls no Kubernetes API (ADR 0035).
- **No authentication on the listener.** Restricting who reaches it is the
  deployment's job.

The metric names are listed in the README under "Metrics".

## Consequences

- **The endpoint is unauthenticated, and account names and their activity are
  readable to anything that reaches it.** The chart renders
  `WORKSPACE_METRICS_ADDR=:<port>`, which listens on every interface of the
  pod: restrict it with a NetworkPolicy that admits only the scraper, or set
  `WORKSPACE_METRICS_ADDR` to one address (`127.0.0.1:9090` for a sidecar or
  `kubectl port-forward`, a private interface elsewhere).
- **Every enrolled account can read it too, and that is accepted** (2026-10-07).
  The listener is in the agent's network namespace, which is where every
  account's shell runs, so neither a NetworkPolicy nor `127.0.0.1` keeps a
  shell out: any account can learn which accounts are connected, which
  `user ls` refuses a non-admin. It reveals names, counts and the version,
  never a key, a token or a file, and authenticating the scrape would cost
  Prometheus' annotation discovery. The threat model's flow 10 has the detail.
- Histogram buckets are fixed in code; changing one is a release, and a
  dashboard's quantiles are only as fine as the buckets.
- `remote_docker_account_daemons` counts daemons started or adopted, not
  probed: asking each daemon per scrape is a `docker inspect` each. One that
  died is counted until it is reset.
- Anything beyond this (exemplars, native histograms, the full Go runtime
  collector, OpenMetrics) is the point at which `client_golang` should be
  weighed again.
