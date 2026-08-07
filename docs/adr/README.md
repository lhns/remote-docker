# Architecture decision records

Each record states a decision, the situation that forced it, and what it costs
us. They are append-only: a decision that stops being true is superseded by a
later record rather than edited, so the reasoning behind the current shape can
always be traced back.

`0001`–`0003` are retrospective. They were decided and defended during the
original build, in `DESIGN.md`; writing them up here means the reasoning
survives that document being superseded.

| # | Title | Status |
|---|---|---|
| [0001](0001-docker-in-docker-over-proxied-socket.md) | Docker-in-Docker over a proxied host socket | Accepted |
| [0002](0002-nfsv3-as-the-file-transport.md) | NFSv3 as the file transport | Accepted |
| [0003](0003-client-serves-workspace-mounts.md) | The client serves, the workspace mounts | Accepted |
| [0004](0004-single-go-binary-client.md) | A single Go binary with SSH and NFS embedded | Accepted |
| [0005](0005-docker-api-proxy-over-cli-wrapper.md) | A Docker API proxy, not a CLI wrapper | Accepted |
| [0006](0006-per-bind-nfs-volumes.md) | Per-bind NFS volumes, not one workspace mount | Accepted |
| [0007](0007-virtual-nfs-export-namespace.md) | A virtual NFS export namespace | Accepted |
| [0008](0008-automatic-port-forwarding.md) | Automatic port forwarding driven by Docker events | Accepted |
| [0009](0009-embedding-the-docker-cli.md) | Embedding the Docker CLI, Buildx and Compose | Accepted |
| [0010](0010-go-ssh-server-agent.md) | A Go SSH server agent, not sshd and sudo | Accepted |
| [0011](0011-one-module-shared-contract.md) | One module with a shared contract package | Accepted |
| [0012](0012-shared-dockerd-across-users.md) | A shared dockerd across users | Accepted, revisit |

## Format

`NNNN-kebab-title.md`, with **Status**, **Context**, **Decision** and
**Consequences**. Consequences is the section that earns the document: it is
where the cost of the decision, and the thing a future reader would otherwise
"simplify" away, is written down.
