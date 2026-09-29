# 0049 — A docker.sock bind names the daemon the container runs on

- Status: Accepted; an exception to [ADR 0039](0039-a-single-file-is-a-one-file-export.md)
  and [ADR 0041](0041-the-workspaces-own-paths.md)
- Date: 2026-09-30

## What forced it

`-v /var/run/docker.sock:/var/run/docker.sock` (kind, CI runners, Testcontainers)
failed with `nfsserve: cannot export /var/run/docker.sock`: every bind source is
a path on the user's machine, and a socket cannot cross a share. Listing it in
`WORKSPACE_DIND_MOUNTS` would mount the PARENT daemon into every account's dind,
which gives any account root over all the others.

## Decision

A bind whose source is exactly `/var/run/docker.sock` (after `path.Clean`, or
after the Git Bash reading of ADR 0040) is never exported. It passes through for
the daemon to resolve in its own filesystem, where that path is the daemon's own
socket:

| mode | what `/var/run/docker.sock` is there | where |
|---|---|---|
| per-account dind | the account's dind, `-H unix:///var/run/docker.sock` | `agent/internal/daemons/plan.go:172-176` |
| shared | the shared dockerd | `agent/internal/supervise/dockerd.go:62` |
| VM (ADR 0025) | the operator's dockerd in shared mode, the account's dind otherwise | as above |
| Swarm (ADR 0013) | the elevated child's dockerd; the node's is at `/var/run/host-docker.sock` and stripped from the child | `agent/internal/elevate/plan.go:158`, `:206-221` |

- **It wins over "this machine wins"** (ADR 0041): a socket here could not be
  exported anyway.
- **Only that path.** `/run/docker.sock` is not mapped: tools name
  `/var/run/docker.sock`, and a fixed path keeps ADR 0041's "the client never
  learns the mode". No workspace-info key.
- **`ParseMounts` refuses it as a `WORKSPACE_DIND_MOUNTS` destination**, beside
  `/rd-sock` and `/var/lib/docker`, so the parent cannot be put there.

## Why it grants nothing new

- The account already controls that daemon: `DOCKER_HOST` in its shell, and
  `serveDockerSocket` (`agent/internal/sshd/session.go`) over SSH. In shared mode
  that daemon is everybody's already (ADR 0012).
- It is not the 2375 finding (`docs/threat-model.md`, E): that was an API
  reachable by every container without asking. This is an explicit `-v`, the
  same opt-in it is on any Docker host.
- The parent daemon is never reachable this way.

## Cost

A user whose own machine has a Docker socket cannot bind it into a workspace
container. It could never have worked: a share carries the name, not the socket.
