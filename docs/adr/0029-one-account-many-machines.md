# 0029: One account, many machines

Accepted, 2026-08-11.

## Context

Somebody enrolled one account and used it from a phone and a PC. The second
machine was refused:

```
reserving 127.0.0.1:30001 on the workspace:
  ssh: tcpip-forward request denied by peer
```

That refusal was not a policy: `Allow` compared the holder by account name and
so permitted a second session of one account. It was the operating system, since
the first machine's listener genuinely held the port, and the message was
`x/crypto`'s fixed string for a failed global request, which can carry no
reason at all.

Behind the port were three more reasons the arrangement could not have worked
even if the bind had:

- the port is written into every managed volume's driver options, and
  `EnsureVolume` treats create as idempotent, so a volume keeps the port it was
  made with;

  **Amended 2026-08-13.** This was named and only half answered.
  `proxy.replaceIfStale` repairs such a volume, but it is reachable only from
  `EnsureVolume` <- the rewriter <- `/containers/create`, and `compose up` on a
  container that ALREADY EXISTS never creates one. So the repair is skipped by
  the commonest way of meeting the problem, and the mount fails with
  `connection refused` against a port nothing on screen explains.

  **Do not read a `connection refused` as evidence of this.** The report that
  prompted the amendment turned out to be a different fault: the volume named
  the port the account still held, so nothing had drifted, and the port was held
  by a session the workspace had never noticed dying. `compose down && up`
  cures a broken mount for a reason that has nothing to do with ports either --
  docker's local driver refcounts mounts, so `down` is what unmounts a bad one.
  See ADR 0032, which carries the correction in full.

  The resolution reverses which record is authoritative: a volume keeps its port
  forever and cannot be re-pointed, so **the volumes are the durable statement
  of what port a machine needs** and `clientports` is a cache of something
  already written down. The agent now reads the port back off a machine's own
  volumes (`workspace.ClientLabel` is what makes them attributable) before
  choosing a port for a machine its record has forgotten. A lost `clientports`
  therefore costs nothing, where it used to cost every volume that machine had.
- volume names were per account, so both machines derive `rd-cwd` for their own
  working directory and the second create silently returns the first's volume;
- the collector deletes any `rd-` volume carrying this account's owner label
  that no container holds, so each machine would garbage-collect the other's,
  and losing one is not a tidy failure: the daemon recreates a missing named
  volume as an empty local one, so the container starts with an empty directory
  where the project should be.

Sequential use was always correct. Concurrent use was the problem.

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
workspace has **already authenticated**. That makes it:

- **stable per machine by construction.** The key is created once per machine
  and reused by every later session, and it changes exactly when that machine's
  enrolment changes, which is precisely when the workspace should treat it as a
  different client;
- **authenticated rather than asserted.** An id the client sent would have to be
  taken on trust, and a client could then claim another machine's port or
  another machine's volumes. The agent derives it from the key that just passed;
- **free of new state.** Nothing to keep, lose or migrate.

It takes the key's wire bytes rather than an `ssh.PublicKey`, so the shared
module goes on depending on nothing (ADR 0021).

### The port is derived for the first machine and allocated for the rest

The uid decides an account's **first** port, exactly as ADR 0003 says, so a
workspace anybody reaches from one computer is on the port it always was and
allocates nothing. `space/accounts.Ports` hands out the rest, records
them in `clientports` beside `uidmap`, and answers `Owns`, which the forward
policy now asks instead of recomputing `PortForUID`. A rule that recomputed
would refuse a port the agent had itself just handed out.

Allocation counts **down** from the top of the range. The mapping is a bijection
over the whole space above `PortBase`, so every port up there is spoken for by
some hypothetical uid and there is no gap to allocate from; starting at the far
end means an allocated port meets a derived one only after tens of thousands of
accounts, and `Reserved` refuses to take a port an account that actually exists
derives.

The client needed no change at all: it has always read `NFSPort` off the wire
from `workspace-info` and never computed it.

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

**A container's files belong to the machine that started it.** A container the
PC created holds a mount to the PC's export. From the phone you can list, stop
and follow it, but you cannot make its files come from the phone, and if the PC
goes away its I/O returns EIO. This is inherent to the files being on one
machine, and is documented rather than engineered around.

**ADR 0003's coordination-free property is given up**, deliberately; its
stability property is kept, re-based from the uid to the client. That ADR is
amended to say so.

**A volume with no client label predates this.** It is left alone by the
collector rather than assumed to be ours, because "no label" is not "mine": an
older session of the other machine may still be using it.

**The same key on two machines makes them one client.** That is a synced
configuration directory, and it collides exactly as two sessions on one machine
would. The remedy is a key each, which is also what enrolment means.

**REQUIRED, and not yet built: an account's machines must not share a compose
project.** Compose names a project after the directory it runs in, so the same
compose file on two machines produces one set of container names, one network
and one set of project labels on the daemon they share. Nothing here notices,
because a container carries the client that made it and no code reads that
before acting on somebody else's.

Both outcomes are bad, and the second is worse for being quiet:

- **Different absolute paths on the two machines**, which is the ordinary case.
  The bind source is part of compose's config hash, so each machine sees the
  other's containers as out of date and recreates them, killing a running
  service and re-pointing it at its own volumes. The other machine then does the
  same. `compose down` on either takes the whole project with it.
- **The same absolute path on both**, which happens whenever two machines share
  a layout. The hash matches, so the second machine reports everything up to
  date and leaves the first machine's containers running, serving the FIRST
  machine's files through the first machine's tunnel. Nobody typed anything
  wrong and nobody is told.

This is the failure `VolumeNameForID` already prevents for volumes, one level
up: the volume carries the client in its NAME precisely because the daemon is
shared while the files are not. Container and project names must be separated
the same way, and until they are, this record is incomplete rather than merely
undocumented. What would satisfy it: a project namespace per machine by default,
and a refusal, or at minimum a warning that names the other machine, when a
create would adopt a project another client owns.

Until it is built the remedy is `COMPOSE_PROJECT_NAME`, or `compose -p`, set
differently on each machine. That is a convention nothing enforces, which is
what makes it a requirement rather than an answer.

*(2026-08-18: knowingly accepted for now. It is written down here and in the
README so that somebody meeting it has a name for it, and so the next person to
open this record does not have to rediscover it.)*

**Rejected: a second account for the phone.** It works today with no code, and
it splits the daemon too: two image caches, two sets of containers, and the
phone cannot see what the PC started, which is most of the value.

## Verification

Unit, with no docker and no daemon: `ports_test.go` covers the first machine
keeping the derived port, a second getting its own, both surviving a restart,
and an allocation skipping a port an existing account derives.
`TestASecondMachineBindsItsOwnPort` covers the policy. `TestVolumeNamesCarryThe
Client` and `TestParseVolumeName` cover the naming, including the old shape.

End to end this needs two clients against one account, which is
`test/two-clients.sh`. Until that has run, this record describes something
proven in parts and not as a whole.
