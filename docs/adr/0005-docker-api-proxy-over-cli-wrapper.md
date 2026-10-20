# 0005. A Docker API proxy, not a CLI wrapper

- Status: Accepted
- Date: 2026-08-07
- Current answer: the proxy decodes `/containers/create`, the error message
  of a start or restart whose share volume could not mount, and the port fields
  of `GET /containers/json` and `GET /containers/{id}/json`, and copies
  everything else through. A shell on the workspace is a stock `ssh`, not a subcommand
  ([ADR 0018](0018-one-way-to-do-each-thing.md)).

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
  `/containers/create` is decoded as a request, and the two port-bearing
  responses below as bounded bodies; everything else is copied through
  without buffering. Get this wrong and the proxy passes `docker ps` while
  failing `docker build`, `docker exec` and `docker logs -f`, which is a worse
  outcome than not working at all. See ADR 0009.
- **Amendment, 2026-10-14: one response is read as well.** A `start` or
  `restart` the daemon answers with a non-2xx `application/json` body is read,
  bounded at 64 KiB, and when its message says a share volume's NFS mount was
  refused (`volumes/rd-<client>-<share>/_data`, `port=<n>`, `connection
  refused`) only `message` is rewritten to name the cause and a `fix:`. The
  status and every other field are kept, and anything that does not match is
  relayed byte for byte. The session supplies the diagnosis from its own
  client id and tunnel port; it never asks the workspace.
- **Amendment, 2026-10-20: two more responses are read, for their ports.** A
  `200 application/json` answer to `GET /containers/json` or `GET
  /containers/{id}/json`, versioned or not, has its port fields rewritten to
  the numbers this machine opens (ADR 0008). Why here: every tool asks the
  workspace's daemon through this proxy, so it is the one place that fixes
  `docker ps`, `docker port`, `compose ps` and an SDK reading the mapped port
  alike. Bounds:
  - Read at most 16 MiB. moby writes JSON without a `Content-Length`, so
    anything over about 2 KiB (an inspect, a list of two containers) arrives
    chunked. `http.ReadResponse` removes the chunking, and a rewritten body is
    sent with its length and no `Transfer-Encoding`. A chunked JSON body is not
    a hijack under the rule above, which is unchanged.
  - Generic JSON only: only the port fields are decoded, so unknown fields
    survive.
  - Any other method, path (`/containers/{id}/json/...`, stats, events),
    status or content type, a body that does not parse, and one over the bound
    are relayed byte for byte.
- The proxy sees every request, which is what makes ADR 0008's automatic port
  forwarding possible at no extra architectural cost.
- Requests we do not understand are forwarded verbatim rather than rejected.
  A proxy that fails closed on unfamiliar traffic would be a worse tool than
  the wrapper it replaces.
- **The proxy replaced the wrapper's `docker` subcommand, not its usefulness
  as a way in.** The way in is a stock `ssh`, which the agent serves to any
  enrolled key; `remote-docker shell` was deleted by ADR 0018.
