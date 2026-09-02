# Architecture decision records

One record, one decision. Everything a decision needed in order to work belongs
in its record; two things that could be revisited independently belong in two.

Records are append-only **while anything they decided is still true**. A later
record that changes an earlier answer is merged back into it, because two
records answering one question means the first now states something untrue. A
record whose decision is entirely dead is deleted rather than kept as a
tombstone: git has it, and a reader scanning this index should not have to step
over it.

`0001`–`0003` are retrospective, decided and defended in `DESIGN.md` during the
original build.

## Foundations

How the thing works at all.

| # | Title |
|---|---|
| [0001](0001-docker-in-docker-over-proxied-socket.md) | Docker-in-Docker over a proxied host socket |
| [0002](0002-nfsv3-as-the-file-transport.md) | NFSv3 as the file transport |
| [0003](0003-client-serves-workspace-mounts.md) | The client serves, the workspace mounts |
| [0004](0004-single-go-binary-client.md) | A single Go binary with SSH and NFS embedded |
| [0005](0005-docker-api-proxy-over-cli-wrapper.md) | A Docker API proxy, not a CLI wrapper |

## Files and the export

Bind mounts become NFS-backed volumes; the export namespace is virtual, its
handles are derived, and what the workspace remembers of it is checked rather
than trusted.

| # | Title | Status |
|---|---|---|
| [0006](0006-per-bind-nfs-volumes.md) | Per-bind NFS volumes, not one workspace mount | `~/workspace` superseded by 0018 |
| [0007](0007-virtual-nfs-export-namespace.md) | A virtual NFS export namespace | amended by 0027, 0029 |
| [0027](0027-restoring-an-export-the-workspace-remembers.md) | Restoring an export the workspace remembers | |
| [0032](0032-the-workspace-is-the-record.md) | An address is stable container-side, and the workspace is the record | |
| [0033](0033-handles-derived-from-the-path.md) | The root handle is derived, the rest are not | |
| [0039](0039-a-single-file-is-a-one-file-export.md) | A single file is a one-file export | |
| [0041](0041-the-workspaces-own-paths.md) | The workspace's own paths are the ones it mounted into the daemon | |
| [0042](0042-mount-consistency-modes.md) | Docker's mount consistency, applied to the NFS mount | |
| [0044](0044-a-delegated-share-is-a-cache.md) | A delegated share is a cache, not a snapshot | |

## Code layout

| # | Title |
|---|---|
| [0021](0021-the-module-layout.md) | The module layout — two axes: modules by side, packages by feature |

## Daemons

Which dockerd serves an account, who supervises it, and what each mode assumes.

| # | Title | Status |
|---|---|---|
| [0012](0012-shared-dockerd-across-users.md) | A shared dockerd across users | still supported; assumes a mutually trusting user set |
| [0019](0019-a-dockerd-per-account.md) | A dockerd per account, behind one SSH port | the default |

## Ports

| # | Title |
|---|---|
| [0008](0008-published-ports-reach-the-client.md) | Published ports reach the client, at the client's own numbers |
| [0038](0038-udp-crosses-the-tunnel.md) | UDP crosses the tunnel |

## The client's shape

What the binary is, what it answers to, and where it can run.

| # | Title |
|---|---|
| [0009](0009-embedding-the-docker-cli.md) | Embedding the Docker CLI, Buildx and Compose |
| [0018](0018-one-way-to-do-each-thing.md) | One way to do each thing |
| [0023](0023-running-where-the-loader-is-not-us.md) | Running where the loader is not us |
| [0024](0024-the-docker-cli-is-the-root.md) | The Docker CLI is the root |
| [0040](0040-git-bash-mangles-argv.md) | Git Bash mangles argv, and the client undoes it |

## Sessions and connections

| # | Title |
|---|---|
| [0015](0015-connections-on-demand.md) | Connections established on demand |
| [0017](0017-a-background-session-per-workspace.md) | A background session per workspace |
| [0028](0028-a-reservation-belongs-to-a-session.md) | A port reservation belongs to a session, not to an account |
| [0029](0029-one-account-many-machines.md) | One account, many machines |

## Watching for changes

| # | Title | Status |
|---|---|---|
| [0014](0014-inotify-does-not-see-client-changes.md) | inotify does not see client-side changes | **Open** for a mount; closed for `delegated` (0044) |
| [0016](0016-replaying-change-events-as-real-syscalls.md) | Replaying change events as real syscalls | |

## The agent, and where a workspace runs

| # | Title |
|---|---|
| [0010](0010-go-ssh-server-agent.md) | A Go SSH server agent, not sshd and sudo |
| [0013](0013-self-elevation-instead-of-a-launcher.md) | Self-elevation instead of a launcher container |
| [0025](0025-the-agent-as-a-guest.md) | The agent as a guest on a machine it does not own |
| [0026](0026-a-machine-is-a-workspace-we-provision.md) | A machine is a workspace we provision |
| [0034](0034-ssh-inside-a-websocket.md) | SSH inside a WebSocket |
| [0035](0035-the-workspace-on-kubernetes.md) | The workspace on Kubernetes |

## Retired numbers

Consolidated into the record that now carries the decision, or deleted. A number
here in an old commit message or comment means:

| # | went to |
|---|---|
| 0011 one module, shared contract | [0021](0021-the-module-layout.md) |
| 0020 one daemon target | [0019](0019-a-dockerd-per-account.md) |
| 0022 answering to the name `docker` | deleted; [0024](0024-the-docker-cli-is-the-root.md) replaced it |
| 0030 a core module for the tunnel | [0021](0021-the-module-layout.md) |
| 0031 if it knows about Docker, it is glue | [0021](0021-the-module-layout.md) |
| 0036 the agent supervises its daemons | [0019](0019-a-dockerd-per-account.md) |
| 0037 the published port belongs to the client | [0008](0008-published-ports-reach-the-client.md) |
| 0043 delegated is a copy | [0044](0044-a-delegated-share-is-a-cache.md), which made it a cache |

## Status

An **Open** record is not a decision. It states a problem that is measured,
unsolved, and worth not rediscovering, and it lists the candidates so the next
attempt starts where the last one stopped. It stays open until something is
accepted or ruled out.

## Format

`NNNN-kebab-title.md`. Required: a `Status` bullet, a `Date` bullet, and a
section stating what the decision costs. A record that has accumulated dated
amendments also carries a **`Current answer:`** bullet, because today's answer
must not require reading a changelog to the end.

Write them **technical and short**: bullets over paragraphs, tables and code
over description, measured numbers over adjectives. Records up to 0029 use
Context / Decision / Consequences and later ones use What forced it / The
decision / What it costs; both are fine, and neither is worth a rewrite on its
own.

The section that earns the document is the cost: where the thing a future reader
would otherwise "simplify" away is written down.
