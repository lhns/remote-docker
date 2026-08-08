# 0005. A Docker API proxy, not a CLI wrapper

- Status: Accepted
- Date: 2026-08-07

## Context

Something has to reconcile the client's view of a path with the daemon's. The
original clients do it by sidestepping the question: `dockerbox docker …`
opens an SSH session, runs `cd ~/workspace && exec docker …` on the workspace,
and lets the remote shell resolve relative paths against the remote mount.

That works, and it is cheap, but it only works for commands the wrapper itself
runs. `docker compose` does not shell out to `docker` — it speaks the Engine
API directly. So do Testcontainers, IDE Docker integrations, `act`, and
anything else with `DOCKER_HOST` in its environment. All of them are excluded
by construction.

The alternative is to accept that paths must be translated, and translate them
at the only place where every client is visible: the API.

## Decision

The client exposes a **local Docker API endpoint** — a named pipe on Windows
(`npipe:////./pipe/remote-docker`), a unix socket elsewhere — and forwards
requests to the remote daemon over the SSH connection. Users point
`DOCKER_HOST` at it.

Most requests pass through untouched. `POST /containers/create` is decoded,
its bind mounts rewritten (ADR 0006), and re-encoded.

## Consequences

- The real `docker` CLI, `docker compose`, Testcontainers and IDE integrations
  all work unmodified. This is the entire point and it is not achievable any
  other way.
- Rewriting happens against a versioned, documented schema instead of against
  the Docker CLI's argument grammar. `-v`, `--mount`, and Compose's several
  spellings of the same thing all converge on the same API fields by the time
  we see them.
- We must track the Engine API. Unknown fields have to survive a decode/encode
  round trip untouched, or a newer client against our proxy silently loses
  configuration. This is the main ongoing cost and the main thing to test.
- **The proxy must be transparent to hijacked and streamed connections, not
  only to request/response.** `/containers/*/attach`, `/exec/*/start` and
  `/session` are HTTP upgrades carrying raw or gRPC traffic; `/build`,
  `/events`, `/logs` and image push/pull are long-lived streams. Only
  `/containers/create` is ever decoded — everything else is copied through
  without buffering. Get this wrong and the proxy passes `docker ps` while
  failing `docker build`, `docker exec` and `docker logs -f`, which is a worse
  outcome than not working at all. See ADR 0009.
- The proxy sees every request, which is what makes ADR 0008's automatic port
  forwarding possible at no extra architectural cost.
- Requests we do not understand are forwarded verbatim rather than rejected.
  A proxy that fails closed on unfamiliar traffic would be a worse tool than
  the wrapper it replaces.
- ~~`remote-docker shell` still exists and still opens an interactive session
  on the workspace.~~ **Corrected by [ADR 0018](0018-one-way-to-do-each-thing.md):**
  `shell` is gone. The point it was making still holds -- the proxy replaced
  the wrapper's `docker` subcommand, not its usefulness as a way in -- but a
  stock `ssh` is what provides the way in now, and the agent still serves it.
