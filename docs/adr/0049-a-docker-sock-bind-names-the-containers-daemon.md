# 0049 — A docker.sock bind names the daemon the container runs on

- Status: Accepted; an exception to [ADR 0039](0039-a-single-file-is-a-one-file-export.md)
  and [ADR 0041](0041-the-workspaces-own-paths.md)
- Date: 2026-09-30

## What forced it

`-v /var/run/docker.sock:/var/run/docker.sock` (kind, CI runners, Testcontainers)
failed with `nfsserve: cannot export /var/run/docker.sock`: a socket cannot
cross a share. Listing it in `WORKSPACE_DIND_MOUNTS` instead would mount the
PARENT daemon into every account's dind: root over every other account.

## Decision

A bind source that is `/var/run/docker.sock` after `path.Clean`, or after the
Git Bash reading of ADR 0040, is never exported. It passes through, as that
POSIX path, and the daemon resolves it in its own filesystem:

| mode | `/var/run/docker.sock` there is | where |
|---|---|---|
| per-account dind | the account's dind (its second `-H`) | `daemons` plan, `command` |
| shared | the shared dockerd (on a VM, ADR 0025, the operator's) | `supervise.DefaultSocket` |
| Swarm (ADR 0013) | the elevated child's dockerd; the node's is `/var/run/host-docker.sock`, stripped from the child | `elevate.DefaultHostSocket`, `childMounts` |

- **It beats "this machine wins"** (ADR 0041): a socket here could not be
  exported anyway.
- **Only that path.** `/run/docker.sock` is not mapped: tools name
  `/var/run/docker.sock`, and one fixed path needs no workspace-info key.
- **`ParseMounts` refuses it as a `WORKSPACE_DIND_MOUNTS` destination**, beside
  `/rd-sock` and `/var/lib/docker`.

## Why it grants nothing new

- The account already controls that daemon: `DOCKER_HOST` in its shell, and
  `serveDockerSocket` over SSH. In shared mode it is everybody's (ADR 0012).
- Not the 2375 exposure (`docs/threat-model.md`, E): that reached every
  container unasked; this is an explicit `-v`, as on any Docker host.
- The parent daemon is never reachable this way.

## Cost

A socket on the user's own machine cannot be bound into a workspace container,
which never worked: a share carries the name, not the socket.
