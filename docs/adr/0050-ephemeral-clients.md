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
  folded like a key file's name. A name that folds to nothing refuses the start.
- **A run is one client process**, which spans many SSH connections because idle
  ones are released (ADR 0015). Keyed on the connection, every idle reconnect
  would rename the port and the volumes under running containers (ADR 0032).
- **The run id**: 128 random bits, hex, minted in `session.Open` and sent
  nowhere but the run request.
- **The run request** `workspace.RunRequest` (`remote-docker-run`): an SSH
  global request, payload the run id, sent after the handshake and before
  anything else.
- **The derived id** `workspace.EphemeralClientID(key wire bytes, run id)`:
  SHA-256 over `remote-docker-run\0`, the key's length and bytes, and the run
  id, cut to 8 hex. `ClientID`'s shape, so volume names and labels are
  unchanged.
- **workspace-info reports it** as `WORKSPACE_CLIENT`, only for an ephemeral
  account, and the client uses it wherever it used `ClientID`.

### What the agent enforces

| on a connection of | the run request | before it | a second one |
|---|---|---|---|
| an account not listed | acknowledged, ignored | everything as before | acknowledged, ignored |
| an ephemeral account | sets the client | `workspace-info`, `workspace-notify`, `workspace-cache` and the reverse forward refused; a shell, the docker socket and local forwards served | refused |

- **One live connection per run.** A second connection naming a live run is
  refused with a `fix:` line. The claim ends with the connection's context,
  which is when the workspace notices (`armDeadPeerDetection`, `wslisten`).
- **Before the run is named the connection has no client**, and that must stay
  a refusal: `Ports.For` answers an empty client with the account's base port,
  so an info served then would silently hand a run another client's export
  port.
- **Takeover needs both halves**: the authenticated key and the run id.

### Compatibility

| client | agent | result |
|---|---|---|
| new | older | refused with no payload; the client carries on as a machine |
| new | new, account not listed | acknowledged; no `WORKSPACE_CLIENT`, so `ClientID` as before |
| older | new, ephemeral account | `workspace-info` refused: `... this connection named no run` / `fix: upgrade remote-docker on this machine` |

A new agent's refusal always carries its reason as the payload. An empty one is
read as an older agent, so a reasonless refusal would silently make the client
a machine.

## Costs

- **Not yet fit to enable.** Until the registry, `Ports.Free` and cleanup land,
  each run is allocated a port that `clientports` keeps for ever, and its
  volumes outlive it. Hence no README row and no changelog entry yet.
- **Every process is a run, a query session included.** `remote status`
  allocates a port per call, and `remote gc` collects only its own run's
  volumes, which is none.
- **One more round trip per connection, for every account**, since the client
  cannot know an account is ephemeral before asking.
- **Exports are not isolated between runs of one account.** `AllowDial` gates
  SSH channels only, so a `--network host` or `docker.sock`-bound (ADR 0049)
  container of the account reaches every run's export. Accepted, as between
  ADR 0029's machines.
- **A reconnect before the workspace notices the previous connection ended is
  refused**, for up to the ~60s of dead-peer detection: the wait ADR 0028's
  port reservation already imposes on a machine.
- **32 bits of id per run.** A collision between two live runs of one account
  is refused as one run: loud, and negligible at a handful of runs.

## Verification

Unit tests only: `core/workspace` (`client_test.go`, `TestInfoClient`),
`agent/internal/sshd/run_test.go`, `TestEphemeralAccounts`, and
`client/internal/session/run_test.go`. `test/ephemeral.sh` arrives with cleanup.
