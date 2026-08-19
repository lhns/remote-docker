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

*(2026-08-11: on a VM (ADR 0025) this mode is a different bargain. Sharing a
dind's daemon means accounts see each other's containers; sharing a machine's
means they also see, and can stop, whatever else that machine runs. It is also
the only mode that needs an NFS client on the machine itself, because there is
no dind image supplying one.)*

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
- **`AllowDial` is not the whole of that, and cannot be.** It gates SSH
  channels. An account also has a shell in the workspace container, in the
  namespace the exports are bound in, and a socket opened there asks no
  forwarding policy at all. So in this mode an enrolled account can speak NFS
  to another account's export while a session is live, and the only answers are
  the trust assumption in the Decision above and running ADR 0019's mode, where
  the export is not in the namespace shells run in. `test/integration.sh`
  measures the reachability and `test/per-user-dind.sh` asserts its absence in
  the default mode.
- **Published ports no longer collide** (ADR 0008, 2026-08-18). Two accounts
  running `-p 8080:80` used to be first come, first served, since a published
  port is bound in this container. The daemon is now asked for any free port and
  each client opens the number its user typed, so both work. TCP only: a UDP
  port is still bound where it was asked for, because the tunnel cannot carry
  it. This is convenience, not isolation, and the trust assumption below is
  unaffected.
- **Compose projects collide too, and that one is not solved** (2026-08-18).
  Two accounts running the same compose file from a directory of the same name
  produce one project on this daemon: one set of container names, one network,
  one set of project labels. Either they recreate each other's containers, or, if
  their paths match, one silently serves the other's files. ADR 0019 mode removes
  it, because there is nothing shared to collide in.
  [ADR 0029](0029-one-account-many-machines.md) states the requirement, since
  the same thing happens between one account's TWO MACHINES and cannot be answered
  by changing modes.
- Revisit when the user set stops being small and mutually trusted. That is the
  trigger; there is no other reason this decision needs to change. The tunnel
  reachability above is a second, smaller one: it is fixed, but it is the kind
  of thing one namespace keeps producing.
- **That trigger is a judgement, not a check.** Nothing can evaluate "small and
  mutually trusted" in CI, so it is the operator's to re-make, not something to
  wait for a build to notice. Said explicitly because the other revisit trigger
  in these records, ADR 0009's, WAS mechanical, went unevaluated for as long as
  it existed, and its stale conclusion was quoted as current fact.

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
and `remote status` reports which one is in force -- the answer changes
whose daemon its other lines describe, and it is not otherwise visible from the
client.
