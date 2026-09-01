# 0043 — Delegated is a copy, filled through a container

- Status: Accepted
- Date: 2026-09-01
- Extends [ADR 0042](0042-mount-consistency-modes.md), which defines the axis
- Current answer: `delegated` is a plain local volume on the workspace, filled
  from this machine before the container is created. It is a snapshot: nothing
  is written back, and nothing refreshes it while a container runs.

## What forced it

ADR 0042 removed the attribute revalidation, and the measurement says what is
left (2026-09-01, 300 files, 160ms RTT):

| mode | read | GETATTR | READ | ACCESS |
|---|---|---|---|---|
| consistent | 291.88s | 1888 | 300 | 422 |
| cached | 98.22s | 13 | 300 | 422 |

The remaining 98s is READ and ACCESS: the file's own bytes and the permission
check, one round trip each. **No attribute cache can remove those** — a live
mount has to fetch what it is asked for. Removing them means not mounting.

## The decision

**A copy, and Docker's own word for one.** `delegated` says the container's
view is authoritative and its reads and writes may be cached, which is exactly
what a local directory on the workspace is.

- **A plain local volume**, under the same name, labels and ownership as every
  other managed volume. So `remote gc`, the collector, the guard and ADR 0029's
  per-client naming are unchanged, and switching modes is the volume recreation
  ADR 0042 already describes.
- **Filled from the client, as one tar stream.** The client reads its own disk
  sequentially, which is the fastest path there is, and the tree crosses the
  link once rather than a round trip per file.
- **Filled through a container, because there is no other way in.** No API
  writes into a volume; `PUT /containers/{id}/archive` is what `docker cp`
  uses. So a container is created with the volume mounted, the tar goes in, and
  it is removed. It is never STARTED: the daemon mounts a created container's
  volumes to resolve the path.
- **Through the caller's own image**, which is the one image the daemon is
  certain to have — the caller is about to run it. Anything else means a pull,
  on a workspace that may have no registry access. A create naming no image is
  refused rather than guessed at.
- **Synchronously, before the container is created.** A container starting
  against a half-populated tree fails in ways nobody can debug.
- **Emptied first**, so the copy is the tree as it is now rather than as it was
  plus everything since deleted. A volume another container is holding cannot be
  emptied and is filled over instead; that is the one case where a deletion here
  does not reach the copy.

## What is deliberately not here

Both are their own decisions, and neither is needed for the mode to be worth
having:

- **Write-back.** What the container writes into the copy stays there and is
  never seen on this machine. The agent could watch the copy with REAL inotify —
  it is a local filesystem, unlike everything ADR 0014 is about — and write
  changes back through the export, with last-writer-wins as the cost.
- **Refresh while a container runs.** The archive API can add and overwrite
  files in a running container, but it cannot delete one, so half a refresh is
  the most it could do. A refresh that is honest about deletions needs the agent
  writing into the volume, which is the same machinery write-back needs.

Until those exist, **`delegated` is a snapshot taken when the container was
created**, and `cached` is the mode for anything watching for changes.

## Consequences

- **A file deleted on this machine is gone from the next container's copy**, and
  present in a running one's. That follows from being a snapshot and is asserted
  in `test/integration.sh` section 15c, in both directions.
- **Disk on the workspace**: one copy per share per client, on top of the graph
  volume. Collected exactly like every other managed volume.
- **The tree crosses the link at container-create time**, so a first `docker
  run` against a large project waits for it. That cost is paid once per
  container rather than per file per read.
- **What a tar cannot carry is skipped**: sockets, devices and pipes, which are
  names on a filesystem rather than objects that travel. The same reasoning as
  ADR 0039's refusal, without the refusal — one socket in a project directory,
  and a running dev server leaves one, must not stop the tree.
- **It works against an agent of any version**, because everything happens
  through the Docker API the proxy already carries. There is no new protocol and
  nothing to negotiate.
