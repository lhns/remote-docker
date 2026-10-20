# 0008 — Published ports reach the client

- Status: Accepted. Consolidates ADR 0037, which changed this record's answer to
  "which number is bound where" rather than adding a decision of its own.
- Date: 2026-08-07, changed 2026-08-18, consolidated 2026-08-19
- Current answer: the daemon publishes **wherever it likes**; the client opens
  **the number the user typed** in front of it, on the machine that typed it.
  Datagrams cross the same way ([ADR 0038](0038-udp-crosses-the-tunnel.md)).

## Context

- `docker run -p 8080:80` publishes on the daemon's network, which for
  Docker-in-Docker is the workspace's namespace, not the client's. Nothing on the
  client can reach it.
- The old workaround was an SSH local forward passed in advance. It fails the
  common case in the most annoying way: `docker compose up` succeeds, the browser
  cannot connect, nothing says why.
- Publishing to the outside world instead means binding on the deployment host:
  collides between users, exposes services asked to be local.
- Then the number itself became scarce. On a shared daemon (ADR 0012) a published
  port is workspace-wide, so two people running `-p 8080:80` collide:
  `Bind for 0.0.0.0:8080 failed: port is already allocated`.
- The alternatives were all bad: per-account daemons (ADR 0019) avoid it but the
  shared daemon is supported and sometimes right; a random port leaves the user
  looking up a new number after every run; a convention about who owns which
  numbers is a rota enforced by nobody.
- Asked from the other end: **who needs the number to be 8080?** Not the
  workspace. The client's local listener is what the browser, the tests and the
  tools connect to.

## The decisions

**Forwards follow the daemon's events.** The proxy already sees every API call
(ADR 0005), so it subscribes to `/events`: a container starting opens a local
listener per published port, stopping closes them. No configuration, no need to
know the ports in advance.

**`HostPort` is emptied on the way through; the client records what it was.** The
daemon is asked for any free port, the client stamps `80/tcp=8080` into a label
and opens `127.0.0.1:8080` in front of whatever came back. Two people asking for
8080 never meet, because neither binds it on the workspace.

**A label, not memory.** Forwards are rebuilt from the daemon's container list on
every reconnect, so nothing else survives a restart to say what was asked for.
Keyed by container port, which is the half that does not change — the published
port is chosen by the daemon and is the thing being looked up.

**One container port published twice is published ONCE.** `-p 8080:80 -p 9090:80`
emptied on both bindings makes them identical; a real daemon then allocates one
port for the pair and fails to bind it twice
(`failed to bind host port 0.0.0.0:32778/tcp: address already in use`), so the
container never starts. Measured in CI, the only place a real daemon answers. One
binding is kept and emptied, the rest dropped, and the client opens every
requested number in front of that single publication. Which requested number
fronts which assigned port does not matter: they all front the same container
port.

**The requested number is honoured only on the machine that asked.** An account's
machines share the daemon and each client forwards the whole account's containers
— that is what lets somebody start a container on the pc and reach it from the
phone ([ADR 0029](0029-one-account-many-machines.md)). The label is a fact about
the machine that typed it, so elsewhere the container is forwarded at the port the
daemon published. Two machines can then both ask for 8080. Without the rule they
contend for one local listener and the winner is whichever reconciliation reached
first, which is a map iteration and therefore a coin toss.

**The refusal moves to the client**, since the scarce resource is now a port on
the user's machine. Reported in the daemon's own words (`port is already
allocated`), because that is the failure being replaced and what tooling matches
on. Asked twice: of the session's own forwards, and of the machine, by opening and
closing a listener.

**Both modes, not only the shared one**, or the client behaves differently
depending on a workspace setting it does not control.

**One case keeps its port**: a binding whose `HostPort` is already empty, which is
the user asking for any port.

## Consequences

- **Published ports are reachable at the address the user asked for.** `-p
  8080:80` means `localhost:8080` on the client.
- **On the workspace, a published port is not the number you chose.** The
  daemon's own `docker ps` there reports what it assigned
  (`docker exec <workspace> docker ps`); anything reaching the service from
  outside the tunnel must look it up. Inside the workspace nothing changes:
  containers reach each other by container port on a shared network. Through
  the client the tools show this machine's number, see the amendment below.
- **Amendment, 2026-10-20: the tools show the local port.** Reading the
  daemon's number in `docker ps` led a person to rewrite a working config to
  it, and an SDK that reads the mapped port from inspect (a fixed-port
  Testcontainers container) dialled a number nothing forwards. The proxy now
  rewrites the ports of `GET /containers/json` and `GET /containers/{id}/json`
  (ADR 0005), by the same `ports.Rule` that opens the forwards, so the display
  and the listeners come from one decision.
  - A container this session forwards shows each local port it opens, one entry
    per number (`-p 8080:80 -p 9090:80` shows both). Inspect also gets
    `HostConfig.PortBindings` back as asked, which the create rewrite emptied.
  - The host is `127.0.0.1` and the daemon's IPv6 duplicate is dropped.
    Judgement call: Docker would show `0.0.0.0`, but the listener here is
    loopback only by design, and showing what is true beats looking familiar.
    Whoever re-makes this call is the maintainer, and nothing re-checks it.
  - A container this session does not forward (another account's on a shared
    daemon) is untouched.
  - **The display is intent, not state.** For a moment after start, or after a
    forward failed because something else holds the port, it shows a port
    that is not open. Mixing requested and workspace numbers by forward state
    would be worse: the same field would mean two things.
  - The forward manager lists containers through its own API client, not this
    proxy, so it keeps seeing the daemon's real ports. A container with the
    Docker socket (ADR 0049) talks to the daemon directly and sees them too,
    which is correct from where it sits.
  - A container dialling the workspace host (`host.docker.internal:8080`)
    reaches the daemon's number, not the one asked for. That was true before
    and the display does not change it.
- **The client binds ports on the user's machine because a container started
  elsewhere.** Every forward opened or closed is reported, never silent.
- **A local port already in use cannot be honoured.** Report and leave it
  unforwarded; never silently move the local listener, which would produce a
  working listener at an address nobody asked for. One user with two stacks
  wanting 8080 still gets an error — from the client now, and the second container
  is not created.
- **The event stream can drop.** Reconciling against `/containers/json` on
  reconnect is required, or forwards leak and containers started during the gap
  are never forwarded.
- **On a shared daemon the event stream carries other users' containers.** Only
  this client's account's are forwarded, or one user's `compose up` opens
  listeners on another user's machine.
- **A container this client did not create is forwarded at the published number.**
  It carries no label; that is the fallback.
- **Nothing about ADR 0012's trust assumption changes.** This removes an everyday
  collision between people who already trust each other.
- **The port was one collision of several.** Container names, compose project
  names and networks are still one namespace per daemon, so the same compose file
  from two machines of one account still collides in every way except the port
  ([ADR 0029](0029-one-account-many-machines.md)).
