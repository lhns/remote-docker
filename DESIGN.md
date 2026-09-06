# Design

Superseded by [`docs/adr/`](docs/adr/).

This file was the design brief for the original shell implementation: a
privileged dind container you SSH into, with `rclone serve nfs` on the client
and a single kernel NFS mount at `~/workspace`. Its reasoning has been moved
into architecture decision records, which cover both what was decided and what
it cost:

| | |
|---|---|
| Why Docker-in-Docker rather than a proxied host socket | [ADR 0001](docs/adr/0001-docker-in-docker-over-proxied-socket.md) |
| Why NFSv3, and what it rules out | [ADR 0002](docs/adr/0002-nfsv3-as-the-file-transport.md) |
| Why the client serves and the workspace mounts | [ADR 0003](docs/adr/0003-client-serves-workspace-mounts.md) |
| Why bind mounts became per-container volumes, retiring the mount-propagation problem this brief spent most of its length on | [ADR 0006](docs/adr/0006-per-bind-nfs-volumes.md) |

One record holds one decision, and a later answer is merged back into the
record that owns the question rather than added beside it. `docs/adr/README.md`
is the index, and carries the numbers that have been retired.
