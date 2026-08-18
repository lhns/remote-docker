# 0008. Automatic port forwarding driven by Docker events

- Status: Accepted
- Date: 2026-08-07

## Context

`docker run -p 8080:80` publishes a port on the daemon's network — which, for
Docker-in-Docker, is the workspace container's own namespace, not the client's.
Nothing on the client can reach it.

Today the user must know the port in advance and pass it to the client as an
SSH local forward (`-Forward 8080:127.0.0.1:8080`). That fails the common case
in the most annoying possible way: you run `docker compose up`, the service
starts correctly, and the browser cannot reach it, with nothing anywhere saying
why.

Publishing to the outside world instead would mean binding ports on the
deployment host, which collides between users and exposes services that were
asked to be local.

## Decision

The proxy already sees every API call (ADR 0005), so it subscribes to the
remote daemon's `/events` stream. When a container starts, it reads the
published-port map and opens a **local listener on the client** for each,
forwarding through the SSH connection to the workspace container's loopback.
When the container stops, the listeners close.

## Consequences

- Published ports are reachable at the same address the user asked for. `-p
  8080:80` means `localhost:8080` on the client, which is what everyone already
  expects and what makes the remote daemon feel local.
- No configuration, and no need to know the ports before starting the client.
  This is what the old design could not do at all.
- The client binds ports on the user's own machine as a side effect of a
  container starting elsewhere. That has to be visible: every forward opened
  or closed is reported, not silent.
- A port already in use on the client cannot be honoured. Report the conflict
  and leave it unforwarded — never silently remap it to a different port, which
  would produce a working listener at an address nobody asked for and quietly
  break the next `docker run` that expected the real one.
- The event stream can drop. Reconciling against `/containers/json` on
  reconnect is required, or forwards leak and containers started during the gap
  are never forwarded.
- With a shared daemon (ADR 0012) the event stream carries other users'
  containers. Only containers this client created should be forwarded, or one
  user's `docker compose up` opens listeners on another user's machine.

## Amended by ADR 0037, 2026-08-18

The local listener and the published port were the same number, which is what
made the workspace side a shared resource: two accounts on one daemon (ADR 0012)
could not both publish 8080. They may now differ. The daemon is asked for any
free port and the client opens the number the user typed in front of it, so
nothing here changes about how a forward is opened or torn down, only what it
is opened at. See [ADR 0037](0037-the-published-port-belongs-to-the-client.md).

