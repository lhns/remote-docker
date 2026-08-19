# 0032 — An address is stable container-side, and the workspace is the record

- Status: Accepted; extends [ADR 0003](0003-client-serves-workspace-mounts.md)
  and [ADR 0029](0029-one-account-many-machines.md)
- Date: 2026-08-13, amended 2026-08-15
- Current answer: never write a client-chosen address into durable workspace
  state unless the agent can reconstruct it. The agent reads a machine's port
  back off its own volumes; `clientports` is a cache, not the record, and a
  record that cannot be read is refused rather than guessed.

Two rules, and the second follows from the first:

> An address only has to be stable between the CONTAINER and the agent.
>
> So a client-chosen address must never be written into durable workspace state
> unless the agent can reconstruct it.

## What forced it

`NFSVolumeOptions` writes `port=N` into a Docker volume's driver options, and
those are immutable — a volume cannot be re-pointed. The port is per machine and
allocated (ADR 0029), and its record could be lost, so a volume can outlive the
only port that would ever mount it. The failure then names a port and nothing
that explains it: `connection refused` against an address nothing on screen
accounts for, on a container that worked yesterday.

**This record was written for a reported failure that turned out to have a
different cause, and the correction matters more than the record does.** The
report was a `docker compose up` reusing an existing container:

```
error while mounting volume '/var/lib/docker/volumes/rd-6dbf.../_data':
... addr=127.0.0.1,port=30001,mountport=30001,... : connection refused
```

Measured afterwards on the workspace that produced it: the volume named port
30001 and the account still held port 30001. **Nothing had drifted.** The port
was not forgotten, it was HELD — by a session the workspace had never noticed
dying, so every reconnect was refused its reverse forward and the client had a
session with no export behind it. That is fixed on the agent side, in
`sshd.armDeadPeerDetection`, and `test/nfs-resilience.sh` reproduces it.

So the hazard below is real and this record stands on it, but it is narrower
than the report suggested and nobody should reach for it to explain a
`connection refused`. Two things bound it, and both are worth knowing before
spending time here:

- Losing the record entirely gives the first machine back `base`, which is
  derived from the uid. **A single-machine account loses nothing.** Only the
  second and later machines of one account hold a port that cannot be
  re-derived.
- Only a volume carrying the client label can be attributed to a machine at all,
  so volumes made before ADR 0029 are outside this entirely.

## Why there is no address to be unstable on the client side

The instinct is to decouple the two halves: keep the container-facing port fixed
and let the client-facing one move. That does not apply, because there is no
client-facing port.

A reverse forward means the AGENT binds the port, inside the account's daemon
namespace (`reversePolicy.Listen` → `netns.Listen`), and the traffic reaches the
client's NFS server over SSH channels. There is exactly ONE port in the path and
it is already on the workspace side. Nothing can absorb a change, so the only
answer is to make that number survive.

## The audit, which bounds the problem to one member

Everything that crosses the boundary, and whether a client-chosen address is
written down anywhere durable:

| crossing | in durable workspace state? |
|---|---|
| the NFS export, backing bind mounts | **yes** — `port=N`, in immutable volume driver options |
| published ports | no — the client forwards to `127.0.0.1:<the container's own published port>`, re-read from the daemon on every reconcile |
| the Docker socket | no — the client dials out, and the agent connects to `target.Socket`, a path on its own side |

One member. So fixing the volume fixes the class, and this record exists to keep
it that way rather than to describe a sweep.

## The decision

**The workspace's own objects are the record, and the agent reconstructs from
them.** A managed volume states the port it was built for; the agent reads it
back before choosing a port for a machine its `clientports` file has forgotten.
`workspace.ClientLabel` is what makes a volume attributable to one machine, and
it moved into the contract for this: the client writes it, the agent reads it,
so both ends must agree on the string.

Three properties this has to keep, and each is a way of being wrong:

- **A failed query changes nothing.** Other `workspace-info` fields use `Lookup,
  never Ensure` because they are displayed and an absent daemon costs a dash.
  This one is ACTED UPON, so it uses `Ensure` — but only for a machine the
  record has forgotten, so the wait is paid once by that machine and never on an
  ordinary connect. When it fails, the port is what it would have been anyway: a
  working session with volumes to rebuild, never no session.
- **A volume with no client label is attributed to nobody.** One predating the
  label cannot be assigned to a machine when an account has two, and a guess
  would hand one machine the other's port.
- **A port another machine holds, or one an account that EXISTS derives, is
  still refused.** Reconstructing what a machine wants does not entitle it to
  take somebody else's, and one listener holds a port.

## Consequences

- **`clientports` becomes a cache.** Losing it costs a query, not a machine's
  volumes. The comment in `Ports.For` admitting that a record it could not write
  "costs it its volumes" is no longer true, which is the point.
- **The agent asks its own daemon a question during login.** That is new
  coupling in a path that used to be pure bookkeeping, bounded by a timeout and
  by only running for an unrecognised machine.
- **The rebuild path stays.** Reconstruction cannot help when the port a
  machine's volumes name has since become another machine's; then the volumes
  genuinely cannot mount and are rebuilt at the current port. Rebuilt, never
  merely removed: the daemon recreates a missing named volume as an empty local
  one, so removing without recreating gives a container an empty directory where
  the project should be.
- **This is the same shape as [ADR 0027](0027-restoring-an-export-the-workspace-remembers.md)**
  and resolved in the opposite direction. There, state outliving a session is
  remembered by the CLIENT, because what outlives it is a capability the
  workspace may name. Here it is remembered by the WORKSPACE, because what
  outlives it is an address the workspace binds. The question to ask of the next
  one is which side owns the thing, not which side is convenient.

## Amendment, 2026-08-15: a record that cannot be read is refused, not guessed

Asking the workspace which port a machine's volumes need means asking that
account's daemon, and a daemon that will not start cannot be asked. That
answered 0, exactly as "this machine has no volumes" does, so the two were the
same value and a workspace whose daemon was down handed the machine the port its
uid derives.

That port is the one another machine is most likely to hold, and it is not the
port this machine's volumes were built for, so it produced either a refused
forward or a set of volumes that would never mount again. Both a long way from
the daemon that was the actual problem: it was measured as one account being
forwarded 65534 in one session and asking for 30000 in the next, while its
daemon restarted every nineteen seconds.

`Preferred` now returns an error when the question could not be put, and `For`
refuses rather than choosing. Nothing is lost by refusing in per-account mode:
the reverse forward is bound inside the very daemon that could not be asked, so
the session was going to fail anyway, three steps later and named after the
wrong thing.
