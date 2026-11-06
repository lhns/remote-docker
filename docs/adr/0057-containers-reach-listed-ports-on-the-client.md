# 0057 — Containers reach listed ports on the client

- Status: Accepted
- Date: 2026-11-06
- Current answer: `host-ports` (`REMOTE_DOCKER_HOST_PORTS`, `remote set
  --host-ports`) lists local TCP ports. For each, the client asks the workspace
  for a reverse forward on the account daemon's default bridge gateway, and
  carries every connection to `localhost:<port>` here. A container created
  through the client gets `host.docker.internal` pointing at that gateway.
  Nothing is forwarded unless listed.

## What forced it

- A container runs in the workspace's dockerd, so `localhost` and the docker
  host are the workspace. A service on the user's machine (an MCP server for an
  AI tool, a dev database) is unreachable, and Docker Desktop users expect
  `host.docker.internal:<port>` to reach it.
- The tunnel already carries one reverse forward, the NFS export, bound inside
  the account's network namespace (ADR 0010, ADR 0019). This is the same
  mechanism with another bind address and another local target.
- Every container of the account can then reach the listed ports, so it cannot
  be on by default.

## The decision

- **Opt in per port, same number on both ends.** A list of TCP ports, nothing
  by default. Remapping (`8000:3000`), UDP and "all ports" are not in this
  record.
- **The address is the daemon's default bridge gateway**, which is what
  `host-gateway` resolves to. `x/crypto/ssh` resolves a listen address locally
  and sends only an IP literal, so a name cannot be asked for.
  - The agent reports it as `WORKSPACE_HOST_GATEWAY` in workspace-info, read
    with `docker network inspect bridge` from the account's own daemon. The key
    is also the capability check: an agent without it is too old.
  - Info asks with `Lookup`, like every field in it, so a daemon that has not
    booted yet answers empty. The client asks again once its NFS tunnel has
    started the daemon, and only then concludes the workspace has none. It
    warns once per process, with a `fix:` line, and carries on without host
    ports.
- **A separate branch in `reversePolicy.Allow`; the loopback rule is not
  loosened.** When the host equals this account's gateway, any port from 1 to
  65535 is allowed (0 is refused: it would pick a number the client never
  named).
  - It comes before `runPort`, or an ephemeral run is told "not this run's
    port".
  - It skips the `ForwardPolicy` reservation map and returns token 0, which
    `Release` treats as never live. With a daemon per account, alice and bob
    both bind `172.17.0.1:3000` in different namespaces, so a `(host, port)` key
    would refuse one of them. The kernel's `EADDRINUSE` in `netns.Listen` is
    the arbiter.
  - The listener binds that IP only, never `0.0.0.0`: in shared mode (ADR 0012)
    that would be the workspace's external interface.
  - Allow asks the account's own daemon for its gateway, so an address that is
    only another account's is refused as any non-loopback host is.
- **The client binds at connect and at every reconnect**, beside the NFS tunnel
  (`session.startHostPorts`).
  - Each accepted connection dials `localhost:<port>`, not `127.0.0.1`: a
    server on this machine may listen on `::1` only.
  - **The client dials only the port that listener was registered for.** It
    never reads a destination from the channel, so a workspace cannot use the
    forward to reach another local port.
  - A port with nothing listening closes the container's connection.
  - A refused bind is one warning per port per connection. It says the
    workspace's log names why and names no cause nobody checked; on a reconnect
    it adds that the workspace may still hold the previous connection's
    listener for about a minute.
- **`/containers/create` adds `host.docker.internal:<gateway>` to
  `HostConfig.ExtraHosts`** only when host ports are forwarded
  (`Rewriter.HostGateway`). It uses the address the forwards listen on, not the
  word `host-gateway`, so it does not depend on how the daemon resolves that
  word.
  - Skipped when the name is already present (a Docker Desktop compose file
    often has it, and keeps what it asked for) and when `NetworkMode` is `host`
    or `container:<id>`, which the daemon refuses to combine with extra hosts.
  - Nothing else in `HostConfig` is touched, and a workspace that reports no
    gateway adds nothing.
- **The idle release already covers it.** `hasLiveDependents` counts any running
  container carrying this account's and machine's labels, and the rewrite
  stamps both on every container it creates, mounts or not. A container using a
  host port holds no gate lease, but it is counted, so the connection and its
  forwards are not released under it.
- **`remote status`** lists each port: forwarded, or why not.

## What it costs

- **Every container of the account reaches the listed ports**, and with a shared
  daemon (ADR 0012) so does every account's container and every shell on the
  workspace. `docs/threat-model.md` flow 12 says what a listed port exposes.
  Whatever listens there must be safe to reach from a container, and anything
  that authenticates by being on `localhost` is not.
- **A build's `RUN` steps do not reach it.** `docker build` creates its
  containers through BuildKit, not `/containers/create`, so no extra host is
  added. Only the user's own `--add-host host.docker.internal:host-gateway`
  works there.
- **The bridge gateway is the daemon's default bridge.** A container on a
  user-defined network reaches it as it reaches any other address of the
  workspace (an integration suite proves it rather than assuming it).
- **`--host-gateway-ip` is not read.** With it set in `WORKSPACE_DOCKERD_ARGS`,
  `host-gateway` resolves to that address and not to the bridge gateway (read
  in the dockerd reference, 2026-11-06; re-check with `curl -s
  https://docs.docker.com/reference/cli/dockerd/ | grep -o 'host-gateway-ip[^<]*' | head`.
  Nobody here has run it).
  The names this client injects are unaffected, since they carry the gateway
  address. A compose file's own `host.docker.internal:host-gateway` is skipped
  as already present and would then miss the listeners.
- **TCP only.** UDP is a follow-up.
- **Two machines of one account can both list 3000, and one wins.** With a
  daemon per account they bind one gateway, so the second is refused by
  `EADDRINUSE`, and its warning says only that the workspace refused it.
- **Old workspaces are named, not worked around.** A workspace that does not
  report a gateway keeps working for everything else.
