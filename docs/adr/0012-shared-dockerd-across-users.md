# 0012. A shared dockerd across users

- Status: Accepted; superseded in part by
  [ADR 0019](0019-a-dockerd-per-account.md)
- Date: 2026-08-07

## Context

One workspace container runs one dockerd, and every enrolled account connects
to it. Accounts are separated as unix users — separate uids, separate home
directories, separate reverse-tunnel ports — but they share the daemon.

Sharing a daemon means sharing everything the daemon owns. Every user can list,
inspect, exec into, stop and remove every other user's containers, read their
images and their volumes, and see their environment variables. Membership of
the `docker` group is, as it always is, equivalent to root on the daemon's host
— here, the workspace container.

The alternative is one workspace container per user: one Swarm service each,
one dockerd each, one `/var/lib/docker` each. That is genuine isolation.

## Decision

Keep the shared daemon, and state the assumption it rests on: **everyone
enrolled in a given workspace is mutually trusted.** Enrolment is already
out-of-band — someone with access to the deployment drops a `.pub` into the
keys directory — so the trust decision is made by a human at the moment of
enrolment.

## Consequences

- One container, one image pull, one layer cache, one `/var/lib/docker`. The
  shared layer cache is a real benefit and not only a cost saving: cold builds
  across a small team get much faster.
- **No isolation between users at the daemon level.** This must be documented
  where people enrolling users will read it, not only here. A workspace is a
  shared machine, and should be treated as one.
- Automatic port forwarding (ADR 0008) must filter the event stream to
  containers this client created. Without that, one user's `docker compose up`
  opens listeners on another user's machine.
- Per-user isolation remains available without redesign: run one workspace
  service per user. The uid-derived port scheme and the account model work
  unchanged with one account per container. What changes is deployment cost,
  not architecture.
- **One network namespace, so one account can reach another's reverse tunnel.**
  Found by the threat model, after this mode had stopped being the default.
  Every account's tunnel listens on loopback in the agent's namespace, so
  `127.0.0.1:<their port>` resolves from any account's session, and what
  answers is their NFS export with AuthFlavorNull: read and write access to
  the files on their machine. Binding another's port was already refused;
  dialling it was not. `ForwardPolicy.AllowDial` now refuses a port another
  account holds, and the shared-mode suite covers it. ADR 0019's mode never had
  this, because each tunnel binds inside its own namespace.
- Revisit when the user set stops being small and mutually trusted. That is the
  trigger; there is no other reason this decision needs to change. The tunnel
  reachability above is a second, smaller one: it is fixed, but it is the kind
  of thing one namespace keeps producing.

## Superseded in part by ADR 0019

[ADR 0019](0019-a-dockerd-per-account.md) gives each account its own dockerd,
which removes the second consequence above as the everyday experience: accounts
no longer see each other's containers, images or volumes.

**It does not satisfy the revisit trigger, and this record is not retired.**
Each per-account daemon runs privileged, so a determined account can still
break out and reach another's. What 0019 buys is that nobody does so by
accident. The assumption stated in the Decision above -- everyone enrolled in a
workspace is mutually trusted -- still holds, and a workspace is still a shared
machine.

The paragraph about one workspace container per user therefore stands unchanged
and is still the answer for a user set that is not mutually trusting. So does
the note about the shared layer cache being a real benefit: 0019 gives that up,
and lists it as a cost rather than pretending otherwise.

Either mode is a supported configuration, chosen by `WORKSPACE_PER_USER_DIND`,
and `remote-docker status` reports which one is in force -- the answer changes
whose daemon its other lines describe, and it is not otherwise visible from the
client.
