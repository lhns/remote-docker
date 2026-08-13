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
| [0006](0006-per-bind-nfs-volumes.md) | Per-bind NFS volumes, not one workspace mount | Accepted; `~/workspace` superseded by 0018 |
| [0007](0007-virtual-nfs-export-namespace.md) | A virtual NFS export namespace | Accepted |
| [0008](0008-automatic-port-forwarding.md) | Automatic port forwarding driven by Docker events | Accepted |
| [0009](0009-embedding-the-docker-cli.md) | Embedding the Docker CLI, Buildx and Compose | Accepted |
| [0010](0010-go-ssh-server-agent.md) | A Go SSH server agent, not sshd and sudo | Accepted |
| [0011](0011-one-module-shared-contract.md) | One module with a shared contract package | Accepted; the single module is superseded by 0021, the contract rule stands |
| [0012](0012-shared-dockerd-across-users.md) | A shared dockerd across users | Accepted; superseded in part by 0019, and an implementation rather than a nil since 0020 |
| [0013](0013-self-elevation-instead-of-a-launcher.md) | Self-elevation instead of a launcher container | Accepted |
| [0014](0014-inotify-does-not-see-client-changes.md) | inotify does not see client-side changes | **Open**, narrowed |
| [0015](0015-connections-on-demand.md) | Connections established on demand | Accepted |
| [0016](0016-replaying-change-events-as-real-syscalls.md) | Replaying change events as real syscalls | Accepted; amended by 0019 |
| [0017](0017-a-background-session-per-workspace.md) | A background session per workspace | Accepted; `up` superseded by 0018 |
| [0018](0018-one-way-to-do-each-thing.md) | One way to do each thing | Accepted |
| [0019](0019-a-dockerd-per-account.md) | A dockerd per account, behind one SSH port | Accepted; routing extracted by 0020 |
| [0020](0020-one-daemon-target.md) | One daemon target, not a mode branch | Accepted |
| [0021](0021-three-modules.md) | Three modules: shared, client, agent | Accepted |
| [0022](0022-answering-to-the-name-docker.md) | The client answers to the name `docker` | **Superseded by 0024** |
| [0023](0023-running-where-the-loader-is-not-us.md) | Running where the loader is not us | Accepted; extends 0004 |
| [0024](0024-the-docker-cli-is-the-root.md) | The Docker CLI is the root | Accepted; supersedes 0022 |
| [0025](0025-the-agent-as-a-guest.md) | The agent as a guest on a machine it does not own | Accepted; extends 0010 |
| [0026](0026-a-machine-is-a-workspace-we-provision.md) | A machine is a workspace we provision | Accepted; extends 0025 |
| [0027](0027-restoring-an-export-the-workspace-remembers.md) | Restoring an export the workspace remembers | Accepted; amends 0007 |
| [0028](0028-a-reservation-belongs-to-a-session.md) | A port reservation belongs to a session, not to an account | Accepted; corrects 0010 |
| [0029](0029-one-account-many-machines.md) | One account, many machines | Accepted; amends 0003, 0007, 0019 |
| [0030](0030-a-core-module-for-the-tunnel.md) | A core module for the tunnel | Accepted; extends 0021 |
| [0031](0031-if-it-knows-about-docker-it-is-glue.md) | If it knows about Docker, it is glue | Accepted; extends 0021, 0030 |
| [0032](0032-the-workspace-is-the-record.md) | An address is stable container-side, and the workspace is the record | Accepted; extends 0003, 0029 |
| [0033](0033-handles-derived-from-the-path.md) | The root handle is derived, the rest are not | Accepted |

An **Open** record is not a decision. It states a problem that is measured,
unsolved, and worth not rediscovering, and it lists the candidates so the next
attempt starts where the last one stopped. It stays open until something is
accepted or ruled out.

## Format

`NNNN-kebab-title.md`, with **Status**, **Context**, **Decision** and
**Consequences**. Consequences is the section that earns the document: it is
where the cost of the decision, and the thing a future reader would otherwise
"simplify" away, is written down.
