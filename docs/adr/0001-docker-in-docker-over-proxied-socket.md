# 0001. Docker-in-Docker over a proxied host socket

- Status: Accepted
- Date: 2026-08-07 (retrospective; decided during the original build)

## Context

Docker is needed on a Windows machine where Docker cannot be installed. A
remote Docker host solves the daemon problem but breaks volume mounts: a
`DOCKER_HOST=ssh://` client resolves bind-mount paths **client-side**, so
`./src:/app` is expanded to `C:\projects\myapp\src` and shipped to a daemon
where that path does not exist. This is docker/compose#8484.

The requirement was explicit: a *real remote filesystem* — WebDAV, NFS, CIFS or
9p — not a copy and not a sync. Two families of solution were considered.

**Copy into a volume.** Create a named volume and `docker cp` into it through a
throwaway `FROM scratch` container. Zero dependencies and the right tool for
shipping build inputs, but useless for live editing.

**Volume plugins.** Docker's built-in `local` driver wraps `mount(8)` and needs
no plugin at all for CIFS or NFS (`--opt type=cifs|nfs`); the rclone volume
plugin covers everything else. Worth recording, because it is a common
misconception: there is no "rclone protocol". The rclone plugin *is* rclone
running on the Docker host, speaking whatever standard backend is configured.
Its manifest declares `"network": {"type": "host"}`, so it can reach a
loopback-forwarded port — that path would have worked.

Both were set aside once it became clear the mount could simply live on the
filesystem the daemon already resolves paths against.

## Decision

Run **Docker-in-Docker** in a privileged container, and put the remote
filesystem mount in the same mount namespace as that daemon.

A proxied host socket cannot work: it resolves bind mounts on the host, where
the files are not. When the daemon and the mount share a namespace, a container
asking for `-v /home/you/workspace/src:/app` gets exactly what it asked for,
with no path translation and no volume driver in the way.

## Consequences

- Every tool that shells out to Docker — Compose, Testcontainers, act and Gitea
  runners, sbt plugins — works unmodified. This is the whole return on the
  decision and the reason it outranks the alternatives.
- The container must be privileged. dind runs its own daemon, sets up its own
  bridge and iptables rules, and mounts filesystems in its own namespace.
- Swarm cannot run privileged tasks, so a Swarm deployment needs a launcher
  that starts the real container through the node's own `docker.sock`. See
  `deploy/swarm-launcher.yml`.
- `/var/lib/docker` wants real local disk. On CephFS- or NFS-backed storage
  `overlay2` refuses outright and `vfs` copies every layer, which is why
  `fuse-overlayfs` is baked into the image and selected with
  `WORKSPACE_DOCKERD_ARGS`.
- Copying into a volume remains the better answer for shipping immutable build
  inputs, and is not ruled out by this decision.
