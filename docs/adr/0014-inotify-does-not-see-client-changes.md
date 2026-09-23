# 0014. inotify does not see client-side changes

- Status: **Open** for a mount — `consistent` and `cached`, narrowed to
  deletions and renames. **CLOSED for `delegated`** (ADR 0044), where there is a
  local filesystem in the path and the event is real rather than approximated.
- Date: 2026-08-07
- Updated: 2026-08-07 once candidate 2 was measured; 2026-09-01 when the union
  closed it for one mode
- Current answer: a share with `write != through` (spelled `delegated` below;
  [ADR 0042](0042-mount-consistency-modes.md) renamed the modes to two axes)
  gets the real event through its union, deletions included. A plain mount gets
  writes and creations replayed as syscalls
  ([ADR 0016](0016-replaying-change-events-as-real-syscalls.md)) and no faithful
  deletion, which is what stays open.

Not a decision: this record states the problem and what is not solved, so it
is not rediscovered. Writes and creates are replayed (ADR 0016) and a union
delivers every event (ADR 0044); what follows is the original problem, still
exactly right about deletions on a plain mount.

## The measurement

A container watching the share two ways at once while a file is created on the
client (`test/probes/watchprobe`, `test/integration.sh` section 11b):

```
RESULT inotify_events=0 poll_entries=1 inotify=[] poll=[created-after-watch.txt]
```

**Polling sees the change; inotify sees zero events.** Polling is the control:
the file is there, so this is a failure of *notification*, not of the mount.

## Why it happens

NFS carries no change notification. The Linux NFS client raises inotify events
only for operations through that mount, so a write the server applies to its
own filesystem is never told to the container's kernel. Inherent to the
protocol; no other NFS server would fix it.

## Why it matters more than it looks

The claim is a *real filesystem, not a sync*, which is worth something only if
changes are **noticed**. vite, webpack, nodemon, air, watchexec and `dotnet
watch` depend on inotify and silently do nothing: the file is there, the tool
is running, nothing happens.

Mutagen, Okteto, Blimp, DDEV and Docker's Synchronized File Shares all answer
this with **file sync into a volume** (README, prior art). This measurement
suggests that is not inertia: sync lands changes as local writes, so inotify
fires. Without a fix, remote-docker is for **builds, tests, one-shot tooling,
and anything that reads files when it starts**, not the edit-reload loop.

## Candidates

1. **Polling watchers.** Works today (`CHOKIDAR_USEPOLLING=1`,
   `WATCHPACK_POLLING=true`, `--poll`), at CPU proportional to the tree, worse
   over a network filesystem. Documented, not injected: silently changing a
   user's build tool is a worse failure than the one it papers over.
2. **Replay the event through the mount. Adopted for writes and creates**
   ([ADR 0016](0016-replaying-change-events-as-real-syscalls.md)). The worry
   that inotify watches are per mount, so a poke through the agent's mount
   would miss a container's, was wrong: marks live on the *inode*, dockerd
   bind-mounts each volume from one NFS mount, and the poke and the container's
   watcher were measured to share `dev` and `ino`. No namespace entering is
   needed. It is the only candidate that keeps the "real filesystem" claim.
3. **FUSE on the container side.** It can generate events for operations it
   performs, but they still have to originate locally: sync again, plus a FUSE
   dependency.
4. **Accept and document.** The README says so at the top.

## What is left

Deletions, and the source half of a rename. `unlink()` of a name the client
already removed fails with `ENOENT` before the kernel generates anything:
`IN_DELETE` requires the file to still be there. The same for
`IN_MOVED_FROM`.

1. **Lie in our own NFS server** (not attempted). A `REMOVE` for a file already
   gone could return success, as Docker Desktop's FUSE client does. But the
   measured `ENOENT` is raised locally in the container's kernel, suggesting
   the VFS short-circuits on a negative dentry before any RPC, in which case
   this cannot work. Worth confirming: it is the only route that closes the gap
   properly.
2. **The coarse approximation** (ships). `REMOTE_DOCKER_WATCH=coarse` pokes
   the parent, giving `IN_MODIFY|IN_ISDIR`: a watcher that rescans notices, one
   that trusts the event kind does not. A setting, because misrepresenting an
   event kind is the user's trade.

## Consequences

- The README leads with this limitation. A user who finds it after an hour of
  a watcher not firing will not trust the rest of the documentation.
- `test/integration.sh` section 11b records the behaviour rather than
  asserting an answer, so if inotify ever fires the suite says so and this
  record is revisited.
- Builds, tests, `docker run`, `docker compose up` and anything reading its
  inputs once are unaffected.
- **A share with `write != through` CLOSES this** (ADR 0044), and nothing else
  does. Every change from this machine is written THROUGH the union, so the
  container's kernel performs a real operation and emits the event itself:
  `IN_MODIFY`, `IN_CLOSE_WRITE` and `IN_DELETE`, measured in
  `test/union-probe.sh` sections 5 and 6d. A plain mount (`write=through`) has
  no local filesystem in the path, and everything above still describes it.
