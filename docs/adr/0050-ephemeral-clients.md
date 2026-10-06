# 0050 — Ephemeral clients

- Status: Accepted. Implemented. The end-to-end suites below first ran green on
  main on 2026-10-06 (commit a87a300).
- Date: 2026-10-05, amended 2026-10-06 (networks through the run's
  containers; `user rm`; suite sections 6b and 8)
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
- **The run id**: 128 random bits, hex, minted by the background session and
  sent nowhere but the run request. A one-off command on the same machine
  (`remote status`, `remote gc`, every `withQuerySession`) asks the endpoint for
  it (`/_remote-docker/run`) and presents it, so it is the same client; with no
  session serving it mints its own.
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

- **A run's port is allocated by its reverse forward, never by workspace-info.**
  Info reports the port on record, or `WORKSPACE_NFS_PORT=0` beside
  `WORKSPACE_CLIENT` for a run that has bound nothing; the hosting client then
  asks `tcpip-forward` for port 0 and is told the one allocated (RFC 4254 7.1).
  So a query allocates nothing.
- **Any number of connections may name one run; one may host it.** A run has
  one port and a port one holder (`ForwardPolicy.Bind`), so a second hosting
  connection is refused at the forward, and a run asking for another run's port
  is refused by `runPort`. The reservation ends when the workspace notices the
  connection end (`armDeadPeerDetection`, `wslisten`).
- **Before the run is named the connection has no client**, and that must stay
  a refusal: `Ports.For` answers an empty client with the account's base port,
  so an info served then would silently hand a run another client's export
  port.
- **Takeover needs both halves**: the authenticated key and the run id, which
  leaves the machine only through the endpoint's own lock and ACL.

### Compatibility

| client | agent | result |
|---|---|---|
| new | older | refused with no payload; the client carries on as a machine |
| new | new, account not listed | acknowledged; no `WORKSPACE_CLIENT`, so `ClientID` as before |
| older | new, ephemeral account | `workspace-info` refused: `... this connection named no run` / `fix: upgrade remote-docker on this machine` |

A new agent's refusal always carries its reason as the payload. An empty one is
read as an older agent, so a reasonless refusal would silently make the client
a machine.

## Decision: a registry of runs, per account

`agent/internal/ephemeral.Registry`, keyed on (account, client id). The run id
is never stored: with the key it takes a run over.

| state | means | becomes |
|---|---|---|
| live | at least one connection named the run | grace when the last ends, or gone if it never bound a port |
| grace | no connection, for at most `WORKSPACE_EPHEMERAL_GRACE` | live on a reconnect; cleaning when it runs out |
| cleaning | expired | gone once `Cleanup` succeeds and the port is freed; a connection naming it is refused until then |

- **Each connection naming a run is counted** from its run request to its end
  (`context.AfterFunc`), so a one-off command joining a live run holds it too.
- **A run that never bound a port ends with its last connection**: nothing can
  name it, so it holds no slot. This is every `remote status` with no session.

### Grace

- `WORKSPACE_EPHEMERAL_GRACE`, default `2m`: longer than the ~60s to notice a
  dead peer over TCP (`armDeadPeerDetection`) or a WebSocket (`wslisten`).
- A reconnect within it reattaches: same client id, same port, same volumes.
- A sweep every quarter of the grace (at least 1s) expires what has run out, so
  a run outlives its last connection by between one and 1.25 graces.

### Limit

- `WORKSPACE_EPHEMERAL_MAX_CLIENTS`, default 8, counts live, grace and cleaning
  runs: an expired run still holds its port and objects until it is gone.
- A new run past it is refused at the run request, with the reason as the
  payload, so the client exits at once naming it:
  `ephemeral: account ci has 8 clients` / `  fix: raise WORKSPACE_EPHEMERAL_MAX_CLIENTS or wait`.
- Either variable unusable refuses the agent's start, naming it.

### Ports

- **The same downward allocator as a second machine's** (`Ports.ForRun`), never
  the account's derived port, and asking `Preferred` first, since a run's
  volumes name its port (ADR 0032).
- **Marked ephemeral and never written to `clientports`**: that file would
  otherwise gain a line per run for ever.
- **Freed by `Ports.Free(account, client, token)`, only after `Cleanup` says
  nothing names the port**, so a volume never mounts another run's export. The
  token is minted per assignment, so a late free for a run since given a port
  again frees nothing (ADR 0028).

### Agent restart

- The registry writes `<state>/ephemeral-runs`, one
  `account:client:port:state:last-seen` line per run with a port. A cache: the
  run's volumes are the record (ADR 0032).
- On start every run has lost its connections: a live one starts its grace
  then, one in grace keeps its deadline, and the sweep cleans the rest. Each is
  given its port back (`Ports.Hold`) unless somebody else now has it, which
  expires it.
- The first sweep runs at start, so what expired while the agent was down is
  cleaned then rather than a quarter of a grace later.

## Decision: cleanup removes only what carries the run's client id

`ephemeral.Cleaner.Clean` is the registry's `Cleanup`, run for every cleaning
run on each sweep. Through `daemons.Targets.Ensure` (a stopped daemon still
holds the run's volumes) and `dockercli.RunObjects`:

| step | what | only when |
|---|---|---|
| 1 | containers labelled with the account (`OwnerLabel`) and the client (`ClientLabel`), `rm -f -v` | `WORKSPACE_EPHEMERAL_CLEANUP_CONTAINERS=true` |
| 2 | networks whose `com.docker.compose.project` is the project of a step-1 container AND ends in `-<client>` | the same; the daemon refuses one with anything attached |
| 3 | `unions.Manager.Release` for the client | always; a union a container is bound to stays (ADR 0044) |
| 4 | volumes with the `rd-` prefix, `ManagedLabel=share`, the account and the client | neither a container names it nor `MountedCaches` lists it |
| 5 | the port, `Ports.Free` | steps 1 to 4 kept nothing |

- **A network is the run's only through its containers.** A network carries
  no owner label, since compose creates it through `/networks/create`, which
  the proxy passes through. On a shared daemon (ADR 0012) another account can
  name a project with the same suffix, so the projects come from the run's
  own labelled containers, read before step 1 removes them. A network whose
  containers are already gone is left behind, which costs a name and nothing
  else.
- **`user rm` drops the account's runs** in per-account mode (ADR 0019):
  their records and ports go at once and nothing is cleaned, because cleaning
  calls `Ensure` and would start the removed account's daemon again, and the
  run's objects went with that daemon. On the shared daemon the runs stay and
  are cleaned as usual.
- **Cannot tell means keep.** A listing that fails, a daemon that cannot be
  reached, or an object the daemon refuses to remove keeps the run in cleaning,
  with its port, and the next sweep tries again.
- **A run whose containers remain keeps its slot.** With the variable off, a
  container of the run names its volumes, so they are kept and the run counts
  against the limit until somebody removes the container.
- **Logs**: one line per state change and per refusal (the limit, a run being
  cleaned), and one per object removed or kept, with why.
- **`remote-dockerd ephemeral ls`** reads `<state>/ephemeral-runs`: account,
  client, state, port, and the time since a connection of the run last started
  or ended. A run past its grace shows `grace` until the next sweep.

## Decision: the embedded compose names a run's projects after it

Runs of one account share a daemon, so one compose file from two runs is one
project (ADR 0029). In an ephemeral run the embedded `docker compose` sets `-p`
to `<name>-<client id>`, where `<name>` is what compose would have chosen: the
file's `name:`, else its directory's.

- **Only when nothing chose a name**: `-p`, `COMPOSE_PROJECT_NAME` and a `.env`
  setting it all win. Compose's own pre-run reads `.env` into the environment,
  so the check runs after it.
- **The client id is asked of the background session**, at
  `/_remote-docker/client`, which connects if it must: it is derived on the
  workspace. Any failure leaves compose's own name, and compose then reports
  the connection itself.
- **A machine is unchanged**: its session reports no client id.
- **The suffix is what cleanup's step 2 matches**, together with a container
  of the run in the project. A project named any other way keeps its networks
  after the run.
- **A standalone `docker compose` is documented, not handled**: it reads its own
  environment and cannot learn the client id.

## Costs

- **A cache volume under an adopted union is kept for ever.** After an agent
  restart a union still serving is not in `unions.Manager`'s record, so
  `Release` does not unmount it and `MountedCaches` still lists its volume.
  Only reachable where dockerd outlives the agent (ADR 0025); the run then
  holds its slot and port until the union is unmounted by hand.
- **Starting a stopped daemon to clean.** `Ensure` boots an account's daemon
  that nobody is using, once, after a workspace restart left runs to clean.
- **An ephemeral client never releases its connection for idleness** (ADR
  0015), since that would start its run's grace under a live process. The run
  ends with the process, at the latest the background session's idle exit
  (ADR 0017, 30 minutes), and then its grace. Machines are released as before.
- **A one-off command with no session running is a run of its own**, so `gc`
  then collects nothing of an earlier run's. It allocates no port.
- **One more round trip per connection, for every account**, since the client
  cannot know an account is ephemeral before asking.
- **Exports are not isolated between runs of one account.** `AllowDial` gates
  SSH channels only, so a `--network host` or `docker.sock`-bound (ADR 0049)
  container of the account reaches every run's export. Accepted, as between
  ADR 0029's machines; the threat model's flow 5 records it.
- **A reconnect before the workspace notices the previous connection ended is
  refused its forward**, for up to the ~60s of dead-peer detection: the wait
  ADR 0028's port reservation already imposes on a machine.
- **32 bits of id per run.** Two live runs of one account colliding share a
  port, and the second is refused its forward: loud, and negligible at a
  handful of runs.

## Verification

Unit: `core/workspace` (`client_test.go`, `TestInfoClient`),
`agent/internal/sshd/run_test.go` (a real SSH conversation; its session half
runs on Linux only), `TestAccountSet`, `client/internal/session/run_test.go`
and `TestRunOfAsksTheBackgroundSession`. The registry: its state machine,
limit and restart record in `agent/internal/ephemeral/registry_test.go`, the
ports in `core-agent/accounts/ports_run_test.go`, the limit and the record over
SSH in `agent/internal/sshd/run_registry_test.go`, `TestEphemeralLimits`, and
the held connection in `client/internal/session/hold_test.go`. Cleanup: its
order and every keep rule against a fake daemon in
`agent/internal/ephemeral/cleanup_test.go`, the first sweep after a restart and
live runs left alone in `registry_test.go`, the network match in
`agent/internal/dockercli/runs_test.go`. The compose default in
`client/cmd/remote-docker/compose_test.go` and the endpoint in
`TestClientNamesTheRunOrSaysWhyNot`; the chart's values in `kubernetes.yml`'s
render step.

End to end, against a real daemon in both modes (`integration.yml`, the two
`ephemeral runs of one key` jobs): `test/ephemeral.sh`, four runs of one key at
one checkout path (a mount namespace each), with a grace of 20s and a limit of
4:

| section | asserts |
|---|---|
| 1 | four client ids, four ports in `ephemeral ls`, none in `clientports`; one share as four `rd-<client>-` volumes; each run's container and concurrent embedded `compose up` reading its own file |
| 2, 3 | a clean end and a `kill -9` each leave nothing with that client id after the grace; the other runs keep reading |
| 4 | an outage shorter than the grace keeps the port and a running mount |
| 5 | an agent restart cleans a killed run and reattaches a live one on its old port and volume |
| 6 | a fifth run refused in under 20s, naming the limit, holding no slot |
| 6b | a clean end and a `kill -9` of a run with a running `write=ephemeral` container: cleanup unmounts that run's union and removes its cache volume; another run's union keeps serving |
| 7 | a machine account's `clientports` line and volumes unchanged throughout |
| 8 | on a workspace with a 180s grace, a black hole on the agent's port puts the run in `grace` once the agent notices (~60s), keeping its port; after it, the run reattaches on the same port and a container started before the block reads its mount again |

`kubernetes.yml`: an unprivileged client Deployment (`restricted` Pod Security
Standard, one Secret key) scaled 1, 4, 1, `docker info` and a bind mount of the
same path in every pod, and only the surviving pod's run and volume after the
grace.
