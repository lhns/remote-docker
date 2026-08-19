# 0015. Connections established on demand

- Status: Accepted
- Date: 2026-08-07

## Context

`remote-docker up` held an SSH connection open for its whole life, with a
keepalive every fifteen seconds, a Docker events stream, and a port-reconcile
ticker. Per workspace.

Multiple workspaces are a supported and encouraged arrangement (ADR 0021,
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

## Corrections, made later

**A stream held no lease.** `DialDocker` released the gate the instant the
stream opened, so a hijacked connection -- `docker attach`, `exec -it`,
`logs -f` -- pinned nothing. Those survived a release only indirectly, because
their container was running and the dependency check noticed it; a `logs -f` on
a **stopped** container had nothing holding the connection and would simply be
cut. The lease now lasts the life of the stream, which is also the reliable
answer to "is anything using this connection": a stream holds its lease for
exactly as long as it is open, while an idle keep-alive connection between
requests holds none. ADR 0017's background session depends on that distinction.

**The dependency check was too broad in one direction.** It matched any volume
with the `rd-` prefix, so on a shared daemon (ADR 0012) another account's
volume pinned this connection open forever -- an idle release that could never
fire, for a dependency that was not ours. It now matches only volumes this
session created, derived from the registry rather than remembered.

**The assertion above did not assert what it says.** Its probe container was
created *through this client*, so it carried our owner label and held one of
our volumes: the connection was pinned on the first check and never released at
all, and the test then confirmed a container was alive that nothing had
threatened. The reconnect path this record calls load-bearing was not exercised
by anything. `test/integration.sh` section 11c now proves a release happens
when nothing depends on the session, *and* that one does not while a container
does.

**A connection could die and nothing noticed.** The gate treated `held` as "this
works", and nothing ever cleared it on failure, so a connection that dropped
between two requests was handed to every request after it. The keepalive did
detect the drop within seconds and closed the SSH client, but it told nobody.
Worse, the release path made the wedge permanent: the sweep asked the *dead*
connection whether anything depended on it, the question failed, and the "cannot
tell is not safe to drop" rule above kept it forever. Recovery needed `remote
restart`, and `restart` itself refused because `IdleFor` asked the same dead
connection.

The rule is unchanged and still right for a connection that is alive. What
changed is that a connection known to be dead is dropped rather than questioned:
`tunnelclient.Client` publishes `Dead`, the gate is given an `alive` check consulted
before every acquire, and both `acquire` and `sweep` drop a dead connection
first. `Status` and `IdleFor` ask `currentLive` rather than `current`, so a
wedged session stops reporting itself ready.

Three consequences worth writing down. A stream in flight at the moment of the
drop is cut and is not resumed: a hijacked stream has no resume point, and
re-attaching silently would produce a log with an invisible hole in it. The
lease counter is deliberately not reset when a dead connection is dropped, since
it counts leases rather than leases on the current connection, and that stream
will still release its own when it closes. And a workspace that is genuinely
down now costs a dial and a timeout on every command, which is what it already
cost when no connection was held.

