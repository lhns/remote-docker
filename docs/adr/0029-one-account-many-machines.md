# 0029 — One account, many machines

- Status: Accepted; amends [ADR 0003](0003-client-serves-workspace-mounts.md),
  [ADR 0007](0007-virtual-nfs-export-namespace.md) and
  [ADR 0019](0019-a-dockerd-per-account.md)
- Date: 2026-08-11, amended 2026-08-13 and 2026-08-18
- Current answer: the account is the identity and the machine is the client,
  keyed on the digest of the enrolled key. The daemon, containers and images are
  shared; the export, its reverse-tunnel port and the volumes naming it are per
  machine. A volume outliving its port is repaired from the workspace's own
  record ([ADR 0032](0032-the-workspace-is-the-record.md)), not from this
  record's `replaceIfStale` alone. Compose project names still collide between
  machines, and the accepted remedy is `COMPOSE_PROJECT_NAME` per machine.

## Context

One account used from a phone and a PC at once. The second machine was refused:

```
reserving 127.0.0.1:30001 on the workspace:
  ssh: tcpip-forward request denied by peer
```

Not a policy (`Allow` compared by account name and permitted it) but the OS:
the first machine's listener held the port, and the message is `x/crypto`'s
fixed string for a failed global request, which carries no reason. Behind the
port, three more things could not have worked even if the bind had:

- **The port is in every managed volume's driver options**, and `EnsureVolume`
  treats create as idempotent, so a volume keeps the port it was made with.
  *Amended 2026-08-13:* `proxy.replaceIfStale` repairs one, but only from
  `/containers/create`, and `compose up` on a container that ALREADY EXISTS
  never creates one, so the mount fails with `connection refused` against an
  unexplained port. The answer ([ADR 0032](0032-the-workspace-is-the-record.md))
  makes **the volumes the durable record of a machine's port** and
  `clientports` a cache: the agent reads the port back off a machine's own
  volumes (attributable by `workspace.ClientLabel`) before choosing one for a
  machine it has forgotten. **Do not read `connection refused` as evidence of
  this**: a port held by a session nobody noticed dying looks the same, and
  `compose down && up` cures a broken mount for a third reason (the local
  driver refcounts mounts). ADR 0032 separates them.
- **Volume names were per account**, so both machines derive `rd-cwd` and the
  second create silently returns the first's volume.
- **The collector deletes any unheld `rd-` volume with this account's owner
  label**, so each machine would collect the other's, and the daemon then
  recreates the missing named volume as an empty local one: the container
  starts with an empty directory where the project should be.

Sequential use was always correct; concurrent use was the problem.

## Decision

**The account is the identity; the machine is the client.** What each of those
owns follows from where the thing physically is:

| Shared between an account's machines | Per machine |
| --- | --- |
| the daemon, and so the containers and images | the NFS export, because the files are on one machine |
| the unix account, its uid, its home, its enrolment | the reverse-tunnel port serving that export |
| published-port forwarding, each client opening its own local listeners | the volumes backing bind mounts, and their driver options |

Sharing the daemon is the point rather than a compromise: start a container on
the PC and watch it from the phone. Sharing files would be meaningless, because
they are not on both.

### The client is the fingerprint of its enrolled key

`workspace.ClientID` is 8 hex characters of the SHA-256 of the public key the
workspace has **already authenticated**, taken as wire bytes so `core` still
depends on nothing (ADR 0021). That makes it:

- **stable per machine**: the key is made once per machine and changes exactly
  when its enrolment does, which is when it should be a different client;
- **authenticated, not asserted**: an id the client sent would let it claim
  another machine's port or volumes;
- **free of new state**: nothing to keep, lose or migrate.

### The port is derived for the first machine and allocated for the rest

- The uid decides an account's **first** port (ADR 0003), so a single-machine
  account is on the port it always was and allocates nothing.
- `core-agent/accounts.Ports` hands out the rest, records them in
  `clientports` beside `uidmap`, and answers `Owns`, which the forward policy
  asks instead of recomputing `PortForUID`: recomputing would refuse a port the
  agent just handed out.
- Allocation counts **down** from `workspace.MaxPort`. The uid mapping is a
  bijection over everything above `PortBase`, so there is no gap to allocate
  from; from the far end an allocated port meets a derived one only after tens
  of thousands of accounts, and `Reserved` refuses a port an existing account
  derives.
- The client needed no change: it has always read `NFSPort` from
  `workspace-info`.

### Concurrent sessions, end to end

1. Both machines authenticate as `alice`. The agent derives a different
   `ClientID` for each from the key each presented.
2. Each asks `workspace-info`. The first is given uid-derived `30001`; the
   second is allocated one and both are written to `clientports`, so each gets
   the same one back on every future connection.
3. Each requests a reverse forward for its own port. `Allow` consults the
   allocation, `Bind` mints a reservation token (ADR 0028), and the two
   listeners coexist in the account's daemon netns.
4. Each rewrites its binds into volumes named `rd-<client>-<share>` with a
   client label, so neither can be handed the other's volume and neither
   collector will delete it.
5. Both see the same containers and images, because there is one daemon.

## Consequences

- **A container's files belong to the machine that started it.** From the
  phone you can list, stop and follow a container the PC created, but its
  mount is the PC's export, and if the PC goes away its I/O returns EIO.
  Inherent, and documented rather than engineered around.
- **ADR 0003's coordination-free property is given up**; its stability
  property is kept, re-based from the uid to the client.
- **A volume with no client label predates this** and is left alone by the
  collector: "no label" is not "mine", and an older session of the other
  machine may be using it.
- **The same key on two machines makes them one client** (a synced config
  directory), and they collide as two sessions on one machine would. The
  remedy is a key each.
- **Rejected: a second account for the phone.** Works with no code, and
  splits the daemon: two image caches, two sets of containers, and the phone
  cannot see what the PC started, which is most of the value.

### An account's machines share one compose project namespace

Compose names a project after its directory, so the same compose file on two
machines is one set of container names, one network and one set of project
labels on the shared daemon. A container carries the client that made it, but
nothing reads that before acting on another machine's.

| paths on the two machines | what happens |
|---|---|
| different (the ordinary case) | the bind source is in compose's config hash, so each machine recreates the other's containers, killing a running service and re-pointing it at its own volumes; `compose down` on either takes the whole project |
| the same | the hash matches, the second machine reports everything up to date, and the FIRST machine's containers go on serving the FIRST machine's files. Nobody is told |

It is the failure `VolumeNameForID` prevents for volumes, one level up.

**Preliminary decision, 2026-08-18: neither fix is built, and the limitation is
accepted.** The remedy is a convention: `COMPOSE_PROJECT_NAME` or
`compose -p`, different per machine, as the README explains.

- *Rejected: namespacing* names per client on the way in and out. It
  virtualises the daemon's namespace, touches responses as well as requests,
  and gives up "only `/containers/create` is ever decoded", which is what makes
  the proxy easy to trust.
- *Rejected: detection.* The quiet case never reaches a create: compose lists
  the project, finds it up to date, and stops. A check on the list would be
  compose-shaped, and its warning would land in the background session's log
  rather than the terminal. A warning nobody reads is not detection.

## Verification

- Unit, no daemon: `core-agent/accounts/ports_test.go` (first machine keeps
  the derived port, a second gets its own, both survive a restart, allocation
  skips a derived port); `TestASecondMachineBindsItsOwnPort` (policy,
  `agent/internal/sshd`); `TestVolumeNamesCarryTheClient` and
  `TestParseVolumeName` (naming, including the old shape, `core/workspace`).
- End to end: `test/two-clients.sh`, a suite in `integration.yml`: sections 4
  (neither refused its tunnel), 5 (a port each, remembered), 6 (each mounts its
  own files), 7 (both see one daemon) and 8 (neither collects the other's
  volumes).
