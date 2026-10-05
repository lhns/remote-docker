# 0050 — Ephemeral clients

- Status: Accepted in part. The identity is implemented; the registry and grace
  period, the per-run port and its release, cleanup, and the compose default
  follow, and this record gains a section for each.
- Date: 2026-10-05
- Amends [ADR 0029](0029-one-account-many-machines.md) for the accounts it names.

## What forced it

Autoscaled CI runners (GitHub ARC) start a pod per job with a random name. Under
ADR 0029 both ways of enrolling them fail:

| enrolment | what breaks |
|---|---|
| one key shared by every pod | every pod is ONE client: the second live session is refused the reverse port (`ForwardPolicy.Bind`), the same checkout path names the same `rd-<client>-<share>` volume, and one pod's collector or union release can remove another's objects |
| a key per pod | an enrolment before every job, and `clientports` grows for ever |

## Decision: identity is per client run

- **Opt-in per account**: `WORKSPACE_EPHEMERAL_ACCOUNTS`, comma-separated,
  folded like a key file's name; a name that folds to nothing refuses the start,
  naming the variable. Read once at agent start.
- **A run is one client process**: the background session of ADR 0017, which
  spans many SSH connections because idle ones are released (ADR 0015). Keyed on
  the connection instead, every idle reconnect would change the port and the
  volume names, which breaks ADR 0032.
- **The client mints a run id** in `session.Open`: 128 random bits, hex, held in
  memory for the life of the process and sent nowhere but the run request.
- **The run request** `workspace.RunRequest` (`remote-docker-run`) is an SSH
  global request whose payload is the run id, sent right after the handshake and
  before `workspace-info`, any forward or any channel.
- **The derived id** is `workspace.EphemeralClientID(key wire bytes, run id)`:
  SHA-256 over `remote-docker-run\0`, the key's length and bytes, and the run id,
  cut to 8 hex characters. Same shape as `ClientID`, so volume names, labels and
  the union's directory checks are unchanged; the prefix keeps it from ever
  being a machine's id.
- **The agent tells the client which id it derived**, as `WORKSPACE_CLIENT` in
  `workspace-info`, emitted only for an ephemeral account. The client uses it
  wherever it used `ClientID`: volume names, `ClientLabel`, the collector, the
  local-port rule.

### What the agent enforces

| on a connection of | the run request | before it | a second one |
|---|---|---|---|
| an account not listed | acknowledged, ignored | everything as today | acknowledged, ignored |
| an ephemeral account | sets the client, once | `workspace-info`, `workspace-notify`, `workspace-cache` and the reverse forward are refused; a shell, the docker socket and local forwards need no client and are served | refused: `this connection already named its run` |

- **One live connection per run.** The agent keeps `(account, client)` for every
  live run and refuses a second connection naming it, with a `fix:` line. The
  entry goes when the connection's context ends, which is when the workspace
  notices (`armDeadPeerDetection`, `wslisten`).
- **Takeover needs both halves**: the derived id needs the authenticated key and
  the run id, and the run id is a bearer secret held by one process.

### Compatibility

| client | agent | result |
|---|---|---|
| new | older | the request is unknown and refused with no payload; the client carries on as a machine |
| new | new, account not listed | acknowledged; `workspace-info` carries no client key, so the client derives `ClientID` as before |
| older | new, ephemeral account | `workspace-info` is refused: `account <name> gives each client run its own identity, and this connection named no run` / `fix: upgrade remote-docker on this machine` |

A refusal from a new agent always carries its reason as the payload, which is
how the client tells it from an older agent's empty one.

## Costs

- **Not yet fit to enable.** Until the registry, `Ports.Free` and cleanup land,
  each run is allocated a port that `clientports` keeps for ever, and its
  volumes outlive it. Hence no README row and no changelog entry yet.
- **Exports are not isolated between runs of one account.** `AllowDial` gates
  SSH channels only, so a `--network host` or `docker.sock`-bound (ADR 0049)
  container of the account reaches every run's export in the daemon's netns.
  Accepted, as between ADR 0029's machines.
- **A run whose previous connection the workspace has not yet noticed ending is
  refused** until it does, up to the ~60s of dead-peer detection. The same wait
  ADR 0028's port reservation already imposes on a reconnecting machine.
- **32 bits of id per run.** Two live runs of one account colliding would be
  refused as one run; at the handful of runs an account holds this is
  negligible, and a collision fails loudly.

## Verification

- `core/workspace/client_test.go`: fixed vectors for `EphemeralClientID`,
  different per run and per key, never a `ClientID`; run id shape.
- `core/workspace/info_test.go` `TestInfoClient`: the key is absent unless set,
  round-trips, and a malformed one is refused.
- `agent/internal/sshd/run_test.go`: a run named once per connection; requests
  needing a client refused before it; an account not listed unaffected; a
  second live connection for one run refused, and accepted once the first ends.
- `agent/cmd/remote-dockerd/serve_test.go` `TestEphemeralAccounts`.
- `client/internal/session/run_test.go`: one run id per process across
  reconnects; an older agent's bare refusal leaves a machine; a reasoned
  refusal is printed; the derived id names volumes and the collector's filter.
- Not yet end to end: `test/ephemeral.sh` arrives with cleanup.
