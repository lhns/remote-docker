# 0015. Connections established on demand

- Status: Accepted
- Date: 2026-08-07

## Context

`remote-docker up` held an SSH connection open for its whole life, with a
keepalive every fifteen seconds, a Docker events stream, and a port-reconcile
ticker. Per workspace.

Multiple workspaces are a supported and encouraged arrangement (ADR 0011,
`context install --all`): install contexts for dev, ci and staging, leave the
sessions running, and switch between them with `docker --context`. Three idle
sessions then cost three processes, three connections and three event streams
for something touched twice a day.

The endpoint and the connection have very different costs, though, and had been
treated as one thing:

| | Cost | Must it persist? |
|---|---|---|
| the endpoint (pipe/socket) | negligible | **yes** — it is how the Docker client finds us |
| SSH, NFS export, port forwards | real | no |

## Decision

Bind the endpoint for the life of the session; establish the connection on the
first request and release it when nothing needs it.

Every Docker request arrives at `Session.DialDocker`, so "connect on first use"
is one place rather than a policy scattered across the session.

**Releasing requires three conditions, not one.** The idle period has elapsed,
no request is in flight, *and* nothing on the workspace still depends on us:

- a container holding one of our volumes has a **live NFS mount**; dropping the
  tunnel gives it `EIO`. `soft,timeo=30` makes that a clean failure rather than
  a hang (ADR 0002), but it is still a failure.
- a running container we created may have **published ports** whose local
  forwards exist only while we are connected.

Both are checked against the daemon's running containers before anything is
released.

**Unable to tell is not the same as safe to drop.** If that check fails, the
connection stays. Holding one costs a socket; dropping one still in use costs a
running container its filesystem.

The policy lives in `connGate[T]`, generic over the connection type, so it is
tested without an SSH client, a daemon or a workspace — a fake connection, a
fake dependency check, and a controllable clock's worth of sleeps.

## Consequences

- An idle workspace costs a bound socket and a sweep timer. Several installed
  contexts stop being something to think about.
- **The first command after an idle period pays a reconnect** — an SSH
  handshake, a `workspace-info` round trip, and re-establishing the reverse
  forward. Roughly a second on a LAN. The idle timeout defaults to a minute so
  that someone working normally never meets it.
- The share registry deliberately outlives any single connection. Share ids are
  derived from the path (ADR 0007), so a reconnect reuses the same exports and
  the same remote volumes rather than orphaning a set per connection.
- Attributes are a wrinkle: the working directory is registered before we know
  the account's uid, because the endpoint must exist before anything can ask us
  to connect. `Registry.SetAttrs` corrects them once the workspace answers.
  Nothing is served in between, so the defaults are never observed.
- `up` says the connection is deferred. "Ready" with nothing connected would
  otherwise look like a lie to anyone who checked with `ss` or `netstat`.
- `status` connects deliberately: reporting what the workspace says is its
  entire job.
- The reconnect path is now load-bearing rather than an error case, which
  makes it worth exercising: an integration assertion starts a container,
  waits out an idle period, and checks its I/O still works.
